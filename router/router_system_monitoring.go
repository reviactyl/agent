package router

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/reviactyl/agent/router/middleware"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	psnet "github.com/shirou/gopsutil/v3/net"
)

// SystemMonitoringSnapshot represents a snapshot of system resources at a point in time
type SystemMonitoringSnapshot struct {
	Timestamp int64               `json:"timestamp"`
	CPU       CPUStats            `json:"cpu"`
	Memory    MemoryStats         `json:"memory"`
	Disk      DiskStats           `json:"disk"`
	Network   NetworkStatsDetails `json:"network"`
	Runtime   RuntimeStats        `json:"runtime"`
}

type CPUStats struct {
	UsagePercent float64   `json:"usage_percent"`
	Cores        int       `json:"cores"`
	PerCore      []float64 `json:"per_core,omitempty"`
}

type MemoryStats struct {
	Total        uint64  `json:"total_bytes"`
	Used         uint64  `json:"used_bytes"`
	Free         uint64  `json:"free_bytes"`
	UsagePercent float64 `json:"usage_percent"`
	Available    uint64  `json:"available_bytes"`
	SwapTotal    uint64  `json:"swap_total_bytes"`
	SwapUsed     uint64  `json:"swap_used_bytes"`
	SwapFree     uint64  `json:"swap_free_bytes"`
	SwapPercent  float64 `json:"swap_usage_percent"`
}

type DiskStats struct {
	Total        uint64  `json:"total_bytes"`
	Used         uint64  `json:"used_bytes"`
	Free         uint64  `json:"free_bytes"`
	UsagePercent float64 `json:"usage_percent"`
	Path         string  `json:"path"`
}

type NetworkStatsDetails struct {
	BytesSent   uint64 `json:"bytes_sent"`
	BytesRecv   uint64 `json:"bytes_recv"`
	PacketsSent uint64 `json:"packets_sent"`
	PacketsRecv uint64 `json:"packets_recv"`
}

type RuntimeStats struct {
	Goroutines int    `json:"goroutines"`
	GoVersion  string `json:"go_version"`
	Arch       string `json:"arch"`
	Uptime     int64  `json:"uptime_seconds"`
}

var startTime = time.Now()

// getSystemMonitoring returns live system monitoring data
func getSystemMonitoring(c *gin.Context) {
	snapshot, err := collectSystemSnapshot()
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, snapshot)
}

// collectSystemSnapshot collects current system resource usage.
func collectSystemSnapshot() (*SystemMonitoringSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		cpuPercent  []float64
		cpuPerCore  []float64
		cpuErr      error
		cpuCoreErr  error
		wg          sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		cpuPercent, cpuErr = cpu.PercentWithContext(ctx, 200*time.Millisecond, false)
	}()
	go func() {
		defer wg.Done()
		cpuPerCore, cpuCoreErr = cpu.PercentWithContext(ctx, 200*time.Millisecond, true)
	}()
	wg.Wait()

	// Total CPU usage
	var totalCPU float64
	if cpuErr == nil && len(cpuPercent) > 0 {
		totalCPU = cpuPercent[0]
	}
	if cpuCoreErr != nil {
		cpuPerCore = []float64{}
	}

	// Memory Stats
	memInfo, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return nil, err
	}

	// Swap Stats (non-fatal — zero out if unavailable)
	swapInfo, _ := mem.SwapMemoryWithContext(ctx)

	// Disk Stats
	diskInfo, err := disk.UsageWithContext(ctx, "/")
	if err != nil {
		// Return zeroed struct rather than failing the entire response.
		diskInfo = &disk.UsageStat{Path: "/"}
	}

	// Network Stats
	netStats, err := psnet.IOCountersWithContext(ctx, false)
	var netInfo NetworkStatsDetails
	if err == nil && len(netStats) > 0 {
		netInfo = NetworkStatsDetails{
			BytesSent:   netStats[0].BytesSent,
			BytesRecv:   netStats[0].BytesRecv,
			PacketsSent: netStats[0].PacketsSent,
			PacketsRecv: netStats[0].PacketsRecv,
		}
	}

	// Runtime Stats
	runtimeInfo := RuntimeStats{
		Goroutines: runtime.NumGoroutine(),
		GoVersion:  runtime.Version(),
		Arch:       runtime.GOARCH,
		Uptime:     int64(time.Since(startTime).Seconds()),
	}

	snapshot := &SystemMonitoringSnapshot{
		Timestamp: time.Now().Unix(),
		CPU: CPUStats{
			UsagePercent: totalCPU,
			Cores:        runtime.NumCPU(),
			PerCore:      cpuPerCore,
		},
		Memory: MemoryStats{
			Total:        memInfo.Total,
			Used:         memInfo.Used,
			Free:         memInfo.Free,
			Available:    memInfo.Available,
			UsagePercent: memInfo.UsedPercent,
			SwapTotal:    func() uint64 { if swapInfo != nil { return swapInfo.Total }; return 0 }(),
			SwapUsed:     func() uint64 { if swapInfo != nil { return swapInfo.Used }; return 0 }(),
			SwapFree:     func() uint64 { if swapInfo != nil { return swapInfo.Free }; return 0 }(),
			SwapPercent:  func() float64 { if swapInfo != nil { return swapInfo.UsedPercent }; return 0 }(),
		},
		Disk: DiskStats{
			Total:        diskInfo.Total,
			Used:         diskInfo.Used,
			Free:         diskInfo.Free,
			UsagePercent: diskInfo.UsedPercent,
			Path:         diskInfo.Path,
		},
		Network: netInfo,
		Runtime: runtimeInfo,
	}

	return snapshot, nil
}
