package main

import (
	"bufio" // Added for line-by-line scanning
	"bytes" // Added for creating a reader from []byte
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-ping/ping"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	gsnet "github.com/shirou/gopsutil/v3/net"
)

const (
	namespace = "node" // Standard for host-level metrics
)

var (
	listenAddress = flag.String("web.listen-address", ":9500", "Address to listen on for web interface and metrics.")
	metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")

	// Ping (using go-ping)
	pingTarget     = flag.String("network.ping-target", "8.8.8.8", "IP address or hostname to ping for latency metrics.")
	pingCount      = flag.Int("network.ping-count", 3, "Number of pings to send per scrape for latency metrics.") // Increased default to 3
	pingInterval   = flag.Duration("network.ping-interval", 1*time.Second, "Interval between pings in a sequence.")
	networkTimeout = flag.Duration("network.timeout", 10*time.Second, "Timeout for network operations (ping, speedtest, iperf).") // Increased default to 10s

	// Speedtest-cli
	speedtestEnabled  = flag.Bool("speedtest.enabled", false, "Enable speedtest-cli metrics.")
	speedtestInterval = flag.Duration("speedtest.interval", 15*time.Minute, "Interval between speedtest-cli runs.")

	// iPerf3
	iperfEnabled  = flag.Bool("iperf.enabled", false, "Enable iperf3 metrics.")
	iperfTarget   = flag.String("iperf.target", "", "iPerf3 server target IP/hostname. Required if iperf.enabled is true.")
	iperfInterval = flag.Duration("iperf.interval", 30*time.Minute, "Interval between iperf3 client runs.")
)

// Metric descriptions (additions for speedtest and iperf)
var (
	// Network related (existing)
	networkBytesReceived      = prometheus.NewDesc(prometheus.BuildFQName(namespace, "network", "receive_bytes_total"), "Total number of bytes received on network interface.", []string{"device"}, nil)
	networkBytesTransmitted   = prometheus.NewDesc(prometheus.BuildFQName(namespace, "network", "transmit_bytes_total"), "Total number of bytes transmitted on network interface.", []string{"device"}, nil)
	networkPacketsReceived    = prometheus.NewDesc(prometheus.BuildFQName(namespace, "network", "receive_packets_total"), "Total number of packets received on network interface.", []string{"device"}, nil)
	networkPacketsTransmitted = prometheus.NewDesc(prometheus.BuildFQName(namespace, "network", "transmit_packets_total"), "Total number of packets transmitted on network interface.", []string{"device"}, nil)
	networkErrorsReceived     = prometheus.NewDesc(prometheus.BuildFQName(namespace, "network", "receive_errs_total"), "Total number of errors encountered while receiving on network interface.", []string{"device"}, nil)
	networkErrorsTransmitted  = prometheus.NewDesc(prometheus.BuildFQName(namespace, "network", "transmit_errs_total"), "Total number of errors encountered while transmitting on network interface.", []string{"device"}, nil)
	networkDroppedReceived    = prometheus.NewDesc(prometheus.BuildFQName(namespace, "network", "receive_drop_total"), "Total number of packets dropped while receiving on network interface.", []string{"device"}, nil)
	networkDroppedTransmitted = prometheus.NewDesc(prometheus.BuildFQName(namespace, "network", "transmit_drop_total"), "Total number of packets dropped while transmitting on network interface.", []string{"device"}, nil)

	tcpConnections = prometheus.NewDesc(prometheus.BuildFQName(namespace, "net", "tcp_connections_total"), "Current number of TCP connections by state.", []string{"state"}, nil)

	pingLatency       = prometheus.NewDesc(prometheus.BuildFQName(namespace, "ping", "latency_seconds"), "Latency to target host in seconds.", []string{"target"}, nil)
	pingPacketsSent   = prometheus.NewDesc(prometheus.BuildFQName(namespace, "ping", "packets_sent_total"), "Total packets sent for ping.", []string{"target"}, nil)
	pingPacketsReceived = prometheus.NewDesc(prometheus.BuildFQName(namespace, "ping", "packets_received_total"), "Total packets received from ping.", []string{"target"}, nil)
	pingPacketLoss    = prometheus.NewDesc(prometheus.BuildFQName(namespace, "ping", "packet_loss_ratio"), "Ratio of packet loss during ping.", []string{"target"}, nil)
	pingUp            = prometheus.NewDesc(prometheus.BuildFQName(namespace, "ping", "up"), "Whether the ping target is reachable (1) or not (0).", []string{"target"}, nil)

	// Speedtest metrics
	speedtestDownloadSpeed = prometheus.NewDesc(prometheus.BuildFQName(namespace, "speedtest", "download_bytes_per_second"), "Last measured download speed from speedtest.net in bytes/second.", []string{"server_name", "server_id"}, nil)
	speedtestUploadSpeed   = prometheus.NewDesc(prometheus.BuildFQName(namespace, "speedtest", "upload_bytes_per_second"), "Last measured upload speed from speedtest.net in bytes/second.", []string{"server_name", "server_id"}, nil)
	speedtestPingLatency   = prometheus.NewDesc(prometheus.BuildFQName(namespace, "speedtest", "ping_latency_seconds"), "Last measured ping latency to speedtest.net server in seconds.", []string{"server_name", "server_id"}, nil)
	speedtestUp            = prometheus.NewDesc(prometheus.BuildFQName(namespace, "speedtest", "up"), "Whether the last speedtest-cli run was successful (1) or not (0).", nil, nil)
	speedtestTimestamp     = prometheus.NewDesc(prometheus.BuildFQName(namespace, "speedtest", "last_run_timestamp_seconds"), "Unix timestamp of the last successful speedtest-cli run.", nil, nil)

	// iPerf3 metrics
	iperfDownloadThroughput = prometheus.NewDesc(prometheus.BuildFQName(namespace, "iperf", "download_bytes_per_second"), "Last measured iperf3 download throughput to target in bytes/second.", []string{"target"}, nil)
	iperfUploadThroughput   = prometheus.NewDesc(prometheus.BuildFQName(namespace, "iperf", "upload_bytes_per_second"), "Last measured iperf3 upload throughput to target in bytes/second.", []string{"target"}, nil)
	iperfRetransmits        = prometheus.NewDesc(prometheus.BuildFQName(namespace, "iperf", "retransmits_total"), "Total retransmits during last iperf3 test.", []string{"target"}, nil)
	iperfUp                 = prometheus.NewDesc(prometheus.BuildFQName(namespace, "iperf", "up"), "Whether the last iperf3 run was successful (1) or not (0).", []string{"target"}, nil)
	iperfTimestamp          = prometheus.NewDesc(prometheus.BuildFQName(namespace, "iperf", "last_run_timestamp_seconds"), "Unix timestamp of the last successful iperf3 run.", []string{"target"}, nil)

	// System related (existing)
	cpuSecondsTotal      = prometheus.NewDesc(prometheus.BuildFQName(namespace, "cpu", "seconds_total"), "Seconds the cpus spent in each mode.", []string{"cpu", "mode"}, nil)
	memoryTotalBytes     = prometheus.NewDesc(prometheus.BuildFQName(namespace, "memory", "total_bytes"), "Total memory in bytes.", nil, nil)
	memoryAvailableBytes = prometheus.NewDesc(prometheus.BuildFQName(namespace, "memory", "available_bytes"), "Available memory in bytes.", nil, nil)
	memoryUsedBytes      = prometheus.NewDesc(prometheus.BuildFQName(namespace, "memory", "used_bytes"), "Used memory in bytes.", nil, nil)
	diskReadBytes        = prometheus.NewDesc(prometheus.BuildFQName(namespace, "disk", "read_bytes_total"), "Total number of bytes read from disk.", []string{"device"}, nil)
	diskWrittenBytes     = prometheus.NewDesc(prometheus.BuildFQName(namespace, "disk", "written_bytes_total"), "Total number of bytes written to disk.", []string{"device"}, nil)
	diskReadCompleted    = prometheus.NewDesc(prometheus.BuildFQName(namespace, "disk", "reads_completed_total"), "Total number of reads completed successfully.", []string{"device"}, nil)
	diskWriteCompleted   = prometheus.NewDesc(prometheus.BuildFQName(namespace, "disk", "writes_completed_total"), "Total number of writes completed successfully.", []string{"device"}, nil)
	diskIoTimeSeconds    = prometheus.NewDesc(prometheus.BuildFQName(namespace, "disk", "io_time_seconds_total"), "Total disk I/O time spent.", []string{"device"}, nil)
	bootTimeSeconds      = prometheus.NewDesc(prometheus.BuildFQName(namespace, "host", "boot_time_seconds"), "Node boot time, in unixtime.", nil, nil)
)

// NetworkCollector implements the prometheus.Collector interface.
type NetworkCollector struct {
	speedtestMtx sync.RWMutex
	speedtestResult *SpeedtestResult

	iperfMtx sync.RWMutex
	iperfResult *IperfResult
}

// Speedtest JSON result structure
type SpeedtestResult struct {
	Type string `json:"type"` // Added to differentiate log entries from result
	Ping struct {
		Latency float64 `json:"latency"`
		Jitter  float64 `json:"jitter"`
	} `json:"ping"`
	Download struct {
		Bandwidth int `json:"bandwidth"` // in bytes/sec
		Bytes     int `json:"bytes"`
		Elapsed   int `json:"elapsed"`
	} `json:"download"`
	Upload struct {
		Bandwidth int `json:"bandwidth"` // in bytes/sec
		Bytes     int `json:"bytes"`
		Elapsed   int `json:"elapsed"`
	} `json:"upload"`
	Server struct {
		ID       int    `json:"id"`
		Name     string `json:"name"`
		Location string `json:"location"`
		Country  string `json:"country"`
	} `json:"server"`
	Timestamp  string  `json:"timestamp"` // ISO 8601
	PacketLoss float64 `json:"packetLoss"` // Add packetLoss from speedtest
	Success bool
}

// iPerf3 JSON result structure (simplified for relevant metrics)
type IperfResult struct {
	Start struct {
		Timestamp struct {
			Timesecs float64 `json:"timesecs"`
		} `json:"timestamp"`
	} `json:"start"`
	End struct {
		SumSent struct {
			Bytes int `json:"bytes"`
			Retransmits int `json:"retransmits"`
			Seconds float64 `json:"seconds"`
		} `json:"sum_sent"`
		SumReceived struct {
			Bytes int `json:"bytes"`
			Seconds float64 `json:"seconds"`
		} `json:"sum_received"`
	} `json:"end"`
	Success bool
	Target string
}


// Describe sends the super-set of all possible descriptors of metrics
// collected by this Collector.
func (c *NetworkCollector) Describe(ch chan<- *prometheus.Desc) {
	// Network interface stats
	ch <- networkBytesReceived
	ch <- networkBytesTransmitted
	ch <- networkPacketsReceived
	ch <- networkPacketsTransmitted
	ch <- networkErrorsReceived
	ch <- networkErrorsTransmitted
	ch <- networkDroppedReceived
	ch <- networkDroppedTransmitted

	// TCP connections
	ch <- tcpConnections

	// Ping latency
	ch <- pingLatency
	ch <- pingPacketsSent
	ch <- pingPacketsReceived
	ch <- pingPacketLoss
	ch <- pingUp

	// Speedtest
	if *speedtestEnabled {
		ch <- speedtestDownloadSpeed
		ch <- speedtestUploadSpeed
		ch <- speedtestPingLatency
		ch <- speedtestUp
		ch <- speedtestTimestamp
	}

	// iPerf3
	if *iperfEnabled {
		ch <- iperfDownloadThroughput
		ch <- iperfUploadThroughput
		ch <- iperfRetransmits
		ch <- iperfUp
		ch <- iperfTimestamp
	}

	// System stats
	ch <- cpuSecondsTotal
	ch <- memoryTotalBytes
	ch <- memoryAvailableBytes
	ch <- memoryUsedBytes
	ch <- diskReadBytes
	ch <- diskWrittenBytes
	ch <- diskReadCompleted
	ch <- diskWriteCompleted
	ch <- diskIoTimeSeconds
	ch <- bootTimeSeconds
}

// Collect gathers metrics from the network interfaces and sends them to
// the provided channel.
func (c *NetworkCollector) Collect(ch chan<- prometheus.Metric) {
	var wg sync.WaitGroup

	// Always collect these
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.collectNetworkInterfaceStats(ch); err != nil {
			log.Printf("Error collecting network interface stats: %v", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.collectTCPConnections(ch); err != nil {
			log.Printf("Error collecting TCP connections: %v", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.collectPingMetrics(ch); err != nil {
			log.Printf("Error collecting ping metrics: %v", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.collectSystemMetrics(ch); err != nil {
			log.Printf("Error collecting system metrics: %v", err)
		}
	}()

	// Conditionally collect cached speedtest metrics
	if *speedtestEnabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.speedtestMtx.RLock()
			result := c.speedtestResult
			c.speedtestMtx.RUnlock()

			if result != nil && result.Success {
				serverID := strconv.Itoa(result.Server.ID)
				// Metrics are in bytes/sec, so they are already in base units for Prometheus.
				// Displaying them in MB/s in logs is for human readability.
				ch <- prometheus.MustNewConstMetric(speedtestDownloadSpeed, prometheus.GaugeValue, float64(result.Download.Bandwidth), result.Server.Name, serverID)
				ch <- prometheus.MustNewConstMetric(speedtestUploadSpeed, prometheus.GaugeValue, float64(result.Upload.Bandwidth), result.Server.Name, serverID)
				ch <- prometheus.MustNewConstMetric(speedtestPingLatency, prometheus.GaugeValue, result.Ping.Latency/1000.0, result.Server.Name, serverID) // latency in ms, convert to seconds
				ch <- prometheus.MustNewConstMetric(speedtestUp, prometheus.GaugeValue, 1)
				if t, err := time.Parse(time.RFC3339Nano, result.Timestamp); err == nil {
					ch <- prometheus.MustNewConstMetric(speedtestTimestamp, prometheus.GaugeValue, float64(t.Unix()))
				}
			} else {
				ch <- prometheus.MustNewConstMetric(speedtestUp, prometheus.GaugeValue, 0)
			}
		}()
	}

	// Conditionally collect cached iperf3 metrics
	if *iperfEnabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.iperfMtx.RLock()
			result := c.iperfResult
			c.iperfMtx.RUnlock()

			if result != nil && result.Success {
				// Metrics are in bytes/sec for Prometheus.
				// Displaying them in MB/s in logs is for human readability.
				downloadSpeedBytesPerSecond := float64(result.End.SumReceived.Bytes) / result.End.SumReceived.Seconds
				uploadSpeedBytesPerSecond := float64(result.End.SumSent.Bytes) / result.End.SumSent.Seconds

				ch <- prometheus.MustNewConstMetric(iperfDownloadThroughput, prometheus.GaugeValue, downloadSpeedBytesPerSecond, result.Target)
				ch <- prometheus.MustNewConstMetric(iperfUploadThroughput, prometheus.GaugeValue, uploadSpeedBytesPerSecond, result.Target)
				ch <- prometheus.MustNewConstMetric(iperfRetransmits, prometheus.CounterValue, float64(result.End.SumSent.Retransmits), result.Target)
				ch <- prometheus.MustNewConstMetric(iperfUp, prometheus.GaugeValue, 1, result.Target)
				ch <- prometheus.MustNewConstMetric(iperfTimestamp, prometheus.GaugeValue, result.Start.Timestamp.Timesecs, result.Target)
			} else if *iperfTarget != "" { // Only report if target is configured
				ch <- prometheus.MustNewConstMetric(iperfUp, prometheus.GaugeValue, 0, *iperfTarget)
			}
		}()
	}

	wg.Wait()
}

func (c *NetworkCollector) collectNetworkInterfaceStats(ch chan<- prometheus.Metric) error {
	netIOCounters, err := gsnet.IOCounters(true) // true for per-interface stats
	if err != nil {
		return fmt.Errorf("could not get net io counters: %w", err)
	}

	for _, stats := range netIOCounters {
		ch <- prometheus.MustNewConstMetric(networkBytesReceived, prometheus.CounterValue, float64(stats.BytesRecv), stats.Name)
		ch <- prometheus.MustNewConstMetric(networkBytesTransmitted, prometheus.CounterValue, float64(stats.BytesSent), stats.Name)
		ch <- prometheus.MustNewConstMetric(networkPacketsReceived, prometheus.CounterValue, float64(stats.PacketsRecv), stats.Name)
		ch <- prometheus.MustNewConstMetric(networkPacketsTransmitted, prometheus.CounterValue, float64(stats.PacketsSent), stats.Name)
		ch <- prometheus.MustNewConstMetric(networkErrorsReceived, prometheus.CounterValue, float64(stats.Errin), stats.Name)
		ch <- prometheus.MustNewConstMetric(networkErrorsTransmitted, prometheus.CounterValue, float64(stats.Errout), stats.Name)
		ch <- prometheus.MustNewConstMetric(networkDroppedReceived, prometheus.CounterValue, float64(stats.Dropin), stats.Name)
		ch <- prometheus.MustNewConstMetric(networkDroppedTransmitted, prometheus.CounterValue, float64(stats.Dropout), stats.Name)
	}
	return nil
}

func (c *NetworkCollector) collectTCPConnections(ch chan<- prometheus.Metric) error {
	conns, err := gsnet.Connections("tcp") // "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6", "unix"
	if err != nil {
		return fmt.Errorf("could not get tcp connections: %w", err)
	}

	stateCounts := make(map[string]int)
	for _, conn := range conns {
		stateCounts[conn.Status]++
	}

	for state, count := range stateCounts {
		ch <- prometheus.MustNewConstMetric(tcpConnections, prometheus.GaugeValue, float64(count), strings.ToLower(state))
	}
	return nil
}

func (c *NetworkCollector) collectPingMetrics(ch chan<- prometheus.Metric) error {
	pinger, err := ping.NewPinger(*pingTarget)
	if err != nil {
		log.Printf("Error creating pinger for %s: %v", *pingTarget, err)
		ch <- prometheus.MustNewConstMetric(pingUp, prometheus.GaugeValue, 0, *pingTarget) // Target down
		return fmt.Errorf("failed to create pinger: %w", err)
	}

	pinger.Count = *pingCount
	pinger.Interval = *pingInterval
	pinger.Timeout = *networkTimeout
	pinger.SetPrivileged(runtime.GOOS != "windows") // Needs elevated privileges on non-Windows for raw sockets.

	ctx, cancel := context.WithTimeout(context.Background(), *networkTimeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		err := pinger.Run() // Blocks until ping is done or timeout
		if err != nil && err.Error() != "context deadline exceeded" && err.Error() != "network unreachable" {
			log.Printf("Ping run error for %s: %v", *pingTarget, err)
		}
	}()

	select {
	case <-ctx.Done():
		log.Printf("Ping to %s timed out after %s", *pingTarget, networkTimeout.String())
		ch <- prometheus.MustNewConstMetric(pingUp, prometheus.GaugeValue, 0, *pingTarget) // Target down due to timeout
		return fmt.Errorf("ping to %s timed out", *pingTarget)
	case <-done:
		// Ping completed
	}

	stats := pinger.Statistics()

	ch <- prometheus.MustNewConstMetric(pingPacketsSent, prometheus.CounterValue, float64(stats.PacketsSent), *pingTarget)
	ch <- prometheus.MustNewConstMetric(pingPacketsReceived, prometheus.CounterValue, float64(stats.PacketsRecv), *pingTarget)
	ch <- prometheus.MustNewConstMetric(pingPacketLoss, prometheus.GaugeValue, stats.PacketLoss/100.0, *pingTarget)

	if stats.PacketsRecv > 0 {
		ch <- prometheus.MustNewConstMetric(pingLatency, prometheus.GaugeValue, stats.AvgRtt.Seconds(), *pingTarget)
		ch <- prometheus.MustNewConstMetric(pingUp, prometheus.GaugeValue, 1, *pingTarget) // Target reachable
	} else {
		ch <- prometheus.MustNewConstMetric(pingUp, prometheus.GaugeValue, 0, *pingTarget) // Target unreachable (no packets received)
	}

	return nil
}

func (c *NetworkCollector) collectSpeedtestMetricsPeriodically() {
	if !*speedtestEnabled {
		return
	}
	for {
		log.Printf("Running speedtest-cli...")
		ctx, cancel := context.WithTimeout(context.Background(), *networkTimeout+2*time.Minute) // Speedtest can take longer
		// Corrected command arguments for Ookla Speedtest CLI v1.2.0.84:
		// Uses --format=json and --accept-license as per the output.
		// --accept-gdpr might also be needed for newer versions, you can add it if issues persist.
		cmd := exec.CommandContext(ctx, "speedtest", "--format=json", "--accept-license")
		output, err := cmd.CombinedOutput()
		cancel()

		var result SpeedtestResult
		result.Success = false // Assume failure until proven otherwise

		if err != nil {
			log.Printf("speedtest-cli command failed: %v, output: %s", err, string(output))
		} else {
			// The speedtest CLI can output multiple JSON objects (e.g., log entries)
			// on separate lines. We need to find the one with "type":"result".
			var foundResult bool
			scanner := bufio.NewScanner(bytes.NewReader(output))
			for scanner.Scan() {
				line := scanner.Bytes()
				var temp map[string]interface{}
				// Try to unmarshal into a generic map to check the "type" field
				if err := json.Unmarshal(line, &temp); err == nil {
					if typ, ok := temp["type"].(string); ok && typ == "result" {
						// Found the result JSON object, now unmarshal it into our SpeedtestResult struct
						if err := json.Unmarshal(line, &result); err != nil {
							log.Printf("Failed to parse 'result' type JSON line from speedtest-cli: %v, line: %s", err, string(line))
						} else {
							result.Success = true
							foundResult = true
							// Log in MB/s for human readability (Bandwidth is in bytes/sec from speedtest-cli)
							log.Printf("Speedtest completed: Download %.2f MB/s, Upload %.2f MB/s, Ping %.2f ms",
								float64(result.Download.Bandwidth)/1000/1000, // bytes/sec to MB/s
								float64(result.Upload.Bandwidth)/1000/1000,   // bytes/sec to MB/s
								result.Ping.Latency)
						}
						break // Once we found the result, we can stop processing lines
					}
					// If it's a "log" type or other non-result JSON, we just ignore it here
				}
			}
			if !foundResult {
				log.Printf("Speedtest run completed but no valid 'result' JSON found in output. Full output: %s", string(output))
				result.Success = false // Explicitly set to false if result wasn't found
			}
		}

		c.speedtestMtx.Lock()
		c.speedtestResult = &result // Store the result (even if unsuccessful)
		c.speedtestMtx.Unlock()

		time.Sleep(*speedtestInterval)
	}
}

func (c *NetworkCollector) collectIperfMetricsPeriodically() {
	if !*iperfEnabled {
		return
	}
	if *iperfTarget == "" {
		log.Printf("iperf3 target not specified. Skipping iperf3 tests.")
		return
	}
	for {
		log.Printf("Running iperf3 client to %s...", *iperfTarget)
		// -J for JSON, -t 10 (10 seconds test)
		// iperf3 -J -c <target> will give both directions in the json output's end.sum_sent and end.sum_received
		ctx, cancel := context.WithTimeout(context.Background(), *networkTimeout+15*time.Second) // iperf test takes time
		cmd := exec.CommandContext(ctx, "iperf3", "-c", *iperfTarget, "-J", "-t", "10")
		output, err := cmd.CombinedOutput()
		cancel()

		var result IperfResult
		result.Success = false
		result.Target = *iperfTarget // Store target for labels

		if err != nil {
			log.Printf("iperf3 client failed to %s: %v, output: %s", *iperfTarget, err, string(output))
		} else {
			if err := json.Unmarshal(output, &result); err != nil {
				log.Printf("Failed to parse iperf3 JSON output for %s: %v, output: %s", *iperfTarget, err, string(output))
			} else {
				// Check for errors within iperf3 output (e.g. server issues)
				// A successful iperf3 run should have bytes transferred in both directions for a typical client-server test.
				// If SumReceived.Bytes or SumSent.Bytes is 0, it indicates an issue or an incomplete test.
				if result.End.SumSent.Seconds == 0 || result.End.SumReceived.Seconds == 0 {
					log.Printf("iperf3 to %s reported zero duration for transfer (likely failed). Output: %s", *iperfTarget, string(output))
				} else {
					result.Success = true
					// Keep Prometheus metrics in bytes/sec. Log in MB/s for human readability.
					downloadSpeedBytesPerSecond := float64(result.End.SumReceived.Bytes) / result.End.SumReceived.Seconds
					uploadSpeedBytesPerSecond := float64(result.End.SumSent.Bytes) / result.End.SumSent.Seconds

					log.Printf("iPerf3 to %s completed: Download %.2f MB/s, Upload %.2f MB/s, Retransmits: %d",
						result.Target,
						downloadSpeedBytesPerSecond/1000/1000, // bytes/sec to MB/s
						uploadSpeedBytesPerSecond/1000/1000,   // bytes/sec to MB/s
						result.End.SumSent.Retransmits)
				}
			}
		}

		c.iperfMtx.Lock()
		c.iperfResult = &result
		c.iperfMtx.Unlock()

		time.Sleep(*iperfInterval)
	}
}


func (c *NetworkCollector) collectSystemMetrics(ch chan<- prometheus.Metric) error {
	// CPU metrics
	cpuTimes, err := cpu.Times(false) // false for aggregate CPU
	if err != nil {
		return fmt.Errorf("could not get CPU times: %w", err)
	}
	if len(cpuTimes) > 0 {
		c := cpuTimes[0]
		ch <- prometheus.MustNewConstMetric(cpuSecondsTotal, prometheus.CounterValue, c.User, "total", "user")
		ch <- prometheus.MustNewConstMetric(cpuSecondsTotal, prometheus.CounterValue, c.System, "total", "system")
		ch <- prometheus.MustNewConstMetric(cpuSecondsTotal, prometheus.CounterValue, c.Idle, "total", "idle")
		ch <- prometheus.MustNewConstMetric(cpuSecondsTotal, prometheus.CounterValue, c.Iowait, "total", "iowait")
		ch <- prometheus.MustNewConstMetric(cpuSecondsTotal, prometheus.CounterValue, c.Steal, "total", "steal")
		ch <- prometheus.MustNewConstMetric(cpuSecondsTotal, prometheus.CounterValue, c.Guest, "total", "guest")
		ch <- prometheus.MustNewConstMetric(cpuSecondsTotal, prometheus.CounterValue, c.Irq, "total", "irq")
		ch <- prometheus.MustNewConstMetric(cpuSecondsTotal, prometheus.CounterValue, c.Softirq, "total", "softirq")
	}

	// Memory metrics
	vmStat, err := mem.VirtualMemory()
	if err != nil {
		return fmt.Errorf("could not get virtual memory stats: %w", err)
	}
	ch <- prometheus.MustNewConstMetric(memoryTotalBytes, prometheus.GaugeValue, float64(vmStat.Total))
	ch <- prometheus.MustNewConstMetric(memoryAvailableBytes, prometheus.GaugeValue, float64(vmStat.Available))
	ch <- prometheus.MustNewConstMetric(memoryUsedBytes, prometheus.GaugeValue, float64(vmStat.Used))

	// Disk I/O metrics (physical partitions only)
	diskIOCounters, err := disk.IOCounters()
	if err != nil {
		return fmt.Errorf("could not get disk io counters: %w", err)
	}
	for device, stats := range diskIOCounters {
		if strings.HasPrefix(device, "loop") || strings.HasPrefix(device, "ram") || strings.HasPrefix(device, "dm-") {
			continue
		}
		ch <- prometheus.MustNewConstMetric(diskReadBytes, prometheus.CounterValue, float64(stats.ReadBytes), device)
		ch <- prometheus.MustNewConstMetric(diskWrittenBytes, prometheus.CounterValue, float64(stats.WriteBytes), device)
		ch <- prometheus.MustNewConstMetric(diskReadCompleted, prometheus.CounterValue, float64(stats.ReadCount), device)
		ch <- prometheus.MustNewConstMetric(diskWriteCompleted, prometheus.CounterValue, float64(stats.WriteCount), device)
		ch <- prometheus.MustNewConstMetric(diskIoTimeSeconds, prometheus.CounterValue, float64(stats.IoTime)/1000.0, device) // IoTime is in ms
	}

	// Boot time
	bootTime, err := host.BootTime()
	if err != nil {
		return fmt.Errorf("could not get boot time: %w", err)
	}
	ch <- prometheus.MustNewConstMetric(bootTimeSeconds, prometheus.GaugeValue, float64(bootTime))

	return nil
}

func main() {
	flag.Parse()

	log.Println("Starting Network Exporter")
	log.Printf("Listening on %s", *listenAddress)
	log.Printf("Metrics exposed on %s", *metricsPath)
	log.Printf("Ping target: %s (Count: %d, Interval: %s, Timeout: %s)", *pingTarget, *pingCount, *pingInterval, *networkTimeout)
	if *speedtestEnabled {
		log.Printf("Speedtest metrics enabled. Interval: %s", *speedtestInterval)
	} else {
		log.Println("Speedtest metrics disabled.")
	}
	if *iperfEnabled {
		if *iperfTarget == "" {
			log.Fatalf("iPerf3 metrics enabled but --iperf.target is not set.")
		}
		log.Printf("iPerf3 metrics enabled. Target: %s, Interval: %s", *iperfTarget, *iperfInterval)
	} else {
		log.Println("iPerf3 metrics disabled.")
	}

	collector := &NetworkCollector{}

	// Start background goroutines for periodic, expensive tests
	if *speedtestEnabled {
		go collector.collectSpeedtestMetricsPeriodically()
	}
	if *iperfEnabled {
		go collector.collectIperfMetricsPeriodically()
	}

	// Create a new registry.
	registry := prometheus.NewRegistry()

	// Register our custom collector.
	registry.MustRegister(collector)

	http.Handle(*metricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { // Corrected: r *http.Request
		w.Write([]byte(`<html>
             <head><title>Network Exporter</title></head>
             <body>
             <h1>Network Exporter</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})

	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
