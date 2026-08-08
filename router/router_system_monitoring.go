package router

import (
	"context"
	"net/http"
	"runtime"
	"strings"
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
	Total        uint64           `json:"total_bytes"`
	Used         uint64           `json:"used_bytes"`
	Free         uint64           `json:"free_bytes"`
	UsagePercent float64          `json:"usage_percent"`
	Path         string           `json:"path"`
	Partitions   []PartitionStats `json:"partitions,omitempty"`
}

type PartitionStats struct {
	Mountpoint   string  `json:"mountpoint"`
	Device       string  `json:"device"`
	Filesystem   string  `json:"filesystem"`
	Total        uint64  `json:"total_bytes"`
	Used         uint64  `json:"used_bytes"`
	Free         uint64  `json:"free_bytes"`
	UsagePercent float64 `json:"usage_percent"`
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

// snapshotCacheTTL is the minimum age a snapshot must reach before it is
// recollected. The panel polls every second while each collection takes
// >=200ms (CPU delta sampling), so concurrent polls would otherwise queue.
var snapshotCacheTTL = 750 * time.Millisecond

var (
	lastSnapshotMu sync.Mutex
	lastSnapshot   *SystemMonitoringSnapshot
	lastSnapshotAt time.Time
)

// getSystemMonitoring returns live system monitoring data
func getSystemMonitoring(c *gin.Context) {
	snapshot, err := cachedSystemSnapshot()
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, snapshot)
}

// ignoredMountPrefixes are mount point prefixes excluded from the disk
// partition report (e.g. "/snap" to hide loop-mounted snap packages).
var ignoredMountPrefixes = []string{"/snap"}

// ignoredMountPoint reports whether the mount point matches any ignore prefix.
func ignoredMountPoint(mountpoint string) bool {
	for _, prefix := range ignoredMountPrefixes {
		if prefix != "" && strings.HasPrefix(mountpoint, prefix) {
			return true
		}
	}

	return false
}

// cachedSystemSnapshot returns the last snapshot if it is younger than
// snapshotCacheTTL, deduplicating overlapping poll requests.
func cachedSystemSnapshot() (*SystemMonitoringSnapshot, error) {
	lastSnapshotMu.Lock()
	defer lastSnapshotMu.Unlock()

	if lastSnapshot != nil && time.Since(lastSnapshotAt) < snapshotCacheTTL {
		return lastSnapshot, nil
	}

	snapshot, err := collectSystemSnapshot()
	if err != nil {
		return nil, err
	}

	lastSnapshot = snapshot
	lastSnapshotAt = time.Now()

	return snapshot, nil
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

	// Per-partition disk stats (non-fatal — pseudo filesystems report 0 total and are skipped).
	partitions, _ := disk.PartitionsWithContext(ctx, false)
	var partitionStats []PartitionStats
	for _, partition := range partitions {
		if ignoredMountPoint(partition.Mountpoint) {
			continue
		}
		usage, err := disk.UsageWithContext(ctx, partition.Mountpoint)
		if err != nil || usage.Total == 0 {
			continue
		}
		partitionStats = append(partitionStats, PartitionStats{
			Mountpoint:   partition.Mountpoint,
			Device:       partition.Device,
			Filesystem:   partition.Fstype,
			Total:        usage.Total,
			Used:         usage.Used,
			Free:         usage.Free,
			UsagePercent: usage.UsedPercent,
		})
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
			Partitions:   partitionStats,
		},
		Network: netInfo,
		Runtime: runtimeInfo,
	}

	return snapshot, nil
}
