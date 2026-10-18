package metrics

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
)

// Snapshot is the stable metrics payload exposed by the v1 API. Byte fields
// are bytes, percentages are in the inclusive range 0-100, and uptime is in
// seconds. CPU usage is a non-blocking delta since the previous CPU sample;
// network counters are cumulative totals aggregated over all interfaces, with
// the same counters also available per interface.
type Snapshot struct {
	Hostname    string        `json:"hostname"`
	OS          OSInfo        `json:"os"`
	CPU         CPU           `json:"cpu"`
	Memory      Memory        `json:"memory"`
	Swap        Swap          `json:"swap"`
	Disk        Disk          `json:"disk"`
	Filesystems []Filesystem  `json:"filesystems"`
	Uptime      uint64        `json:"uptime_seconds"`
	Network     Network       `json:"network"`
	Errors      []MetricError `json:"errors,omitempty"`
}

type MetricError struct {
	Metric  string `json:"metric"`
	Message string `json:"message"`
}

type OSInfo struct {
	Name            string `json:"name"`
	Platform        string `json:"platform"`
	PlatformVersion string `json:"platform_version"`
	KernelArch      string `json:"kernel_arch"`
}

type CPU struct {
	UsagePercent        float64      `json:"usage_percent"`
	LogicalCores        int          `json:"logical_cores"`
	PerCoreUsagePercent []float64    `json:"per_core_usage_percent"`
	LoadAverage         *LoadAverage `json:"load_average,omitempty"`
}

type LoadAverage struct {
	OneMinute      float64 `json:"one_minute"`
	FiveMinutes    float64 `json:"five_minutes"`
	FifteenMinutes float64 `json:"fifteen_minutes"`
}

type Memory struct {
	UsedBytes   uint64  `json:"used_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

type Swap struct {
	TotalBytes  uint64  `json:"total_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

type Disk struct {
	Path        string  `json:"path"`
	UsedBytes   uint64  `json:"used_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

type Filesystem struct {
	Mountpoint     string  `json:"mount_point"`
	Device         string  `json:"device"`
	FilesystemType string  `json:"filesystem_type"`
	UsedBytes      uint64  `json:"used_bytes"`
	TotalBytes     uint64  `json:"total_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

type Network struct {
	BytesReceived   uint64             `json:"bytes_received"`
	BytesSent       uint64             `json:"bytes_sent"`
	PacketsReceived uint64             `json:"packets_received"`
	PacketsSent     uint64             `json:"packets_sent"`
	Interfaces      []NetworkInterface `json:"interfaces"`
}

type NetworkInterface struct {
	Name            string `json:"name"`
	BytesReceived   uint64 `json:"bytes_received"`
	BytesSent       uint64 `json:"bytes_sent"`
	PacketsReceived uint64 `json:"packets_received"`
	PacketsSent     uint64 `json:"packets_sent"`
}

type Collector struct {
	timeout   time.Duration
	diskPath  string
	functions collectorFunctions
}

type collectorFunctions struct {
	hostInfo          func(context.Context) (*host.InfoStat, error)
	cpuPercent        func(context.Context) ([]float64, error)
	cpuPercentPerCore func(context.Context) ([]float64, error)
	logicalCores      func(context.Context) (int, error)
	loadAverage       func(context.Context) (*load.AvgStat, error)
	virtualMemory     func(context.Context) (*mem.VirtualMemoryStat, error)
	swapMemory        func(context.Context) (*mem.SwapMemoryStat, error)
	diskUsage         func(context.Context, string) (*disk.UsageStat, error)
	diskPartitions    func(context.Context) ([]disk.PartitionStat, error)
	networkCounters   func(context.Context) ([]psnet.IOCountersStat, error)
}

const maxFilesystemMounts = 64

func NewCollector(timeout time.Duration, diskPath string) *Collector {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if diskPath == "" {
		diskPath = "/"
	}
	return &Collector{
		timeout:  timeout,
		diskPath: diskPath,
		functions: collectorFunctions{
			hostInfo:          host.InfoWithContext,
			cpuPercent:        func(ctx context.Context) ([]float64, error) { return cpu.PercentWithContext(ctx, 0, false) },
			cpuPercentPerCore: func(ctx context.Context) ([]float64, error) { return cpu.PercentWithContext(ctx, 0, true) },
			logicalCores:      func(ctx context.Context) (int, error) { return cpu.CountsWithContext(ctx, true) },
			loadAverage:       load.AvgWithContext,
			virtualMemory:     mem.VirtualMemoryWithContext,
			swapMemory:        mem.SwapMemoryWithContext,
			diskUsage:         disk.UsageWithContext,
			diskPartitions:    func(ctx context.Context) ([]disk.PartitionStat, error) { return disk.PartitionsWithContext(ctx, true) },
			networkCounters: func(ctx context.Context) ([]psnet.IOCountersStat, error) {
				return psnet.IOCountersWithContext(ctx, true)
			},
		},
	}
}

func (c *Collector) Collect(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var snapshot Snapshot
	var errs []MetricError
	addError := func(metric string, err error) {
		if err != nil {
			errs = append(errs, MetricError{Metric: metric, Message: err.Error()})
		}
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}

	info, err := c.functions.hostInfo(ctx)
	if err != nil {
		addError("host", err)
	} else {
		snapshot.Hostname = info.Hostname
		snapshot.OS = OSInfo{
			Name:            info.OS,
			Platform:        info.Platform,
			PlatformVersion: info.PlatformVersion,
			KernelArch:      info.KernelArch,
		}
		snapshot.Uptime = info.Uptime
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}

	snapshot.CPU.PerCoreUsagePercent = []float64{}
	percent, err := c.functions.cpuPercent(ctx)
	if err != nil {
		addError("cpu", err)
	} else if len(percent) > 0 {
		snapshot.CPU.UsagePercent = percent[0]
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	perCorePercent, err := c.functions.cpuPercentPerCore(ctx)
	if err != nil {
		addError("cpu", err)
	} else {
		snapshot.CPU.PerCoreUsagePercent = append(snapshot.CPU.PerCoreUsagePercent, perCorePercent...)
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	cores, err := c.functions.logicalCores(ctx)
	if err != nil {
		addError("cpu", err)
	} else {
		snapshot.CPU.LogicalCores = cores
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	loadAverage, err := c.functions.loadAverage(ctx)
	if err != nil {
		addError("load_average", err)
	} else if loadAverage == nil {
		addError("load_average", errors.New("load averages unavailable"))
	} else {
		snapshot.CPU.LoadAverage = &LoadAverage{
			OneMinute:      loadAverage.Load1,
			FiveMinutes:    loadAverage.Load5,
			FifteenMinutes: loadAverage.Load15,
		}
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}

	memory, err := c.functions.virtualMemory(ctx)
	if err != nil {
		addError("memory", err)
	} else {
		snapshot.Memory = Memory{
			UsedBytes:   memory.Used,
			TotalBytes:  memory.Total,
			UsedPercent: memory.UsedPercent,
		}
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}

	swap, err := c.functions.swapMemory(ctx)
	if err != nil {
		addError("swap", err)
	} else {
		snapshot.Swap = Swap{
			TotalBytes:  swap.Total,
			UsedBytes:   swap.Used,
			FreeBytes:   swap.Free,
			UsedPercent: swap.UsedPercent,
		}
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}

	usage, err := c.functions.diskUsage(ctx, c.diskPath)
	if err != nil {
		addError("disk", err)
	} else {
		snapshot.Disk = Disk{
			Path:        c.diskPath,
			UsedBytes:   usage.Used,
			TotalBytes:  usage.Total,
			UsedPercent: usage.UsedPercent,
		}
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}

	snapshot.Filesystems = []Filesystem{}
	partitions, err := c.functions.diskPartitions(ctx)
	if err != nil {
		addError("filesystems", err)
	} else {
		partitions, truncated := filterFilesystemPartitions(partitions)
		if truncated {
			addError("filesystems", fmt.Errorf("mount list limited to %d entries", maxFilesystemMounts))
		}
		for _, partition := range partitions {
			if err := ctx.Err(); err != nil {
				return snapshot, err
			}
			usage, err := c.functions.diskUsage(ctx, partition.Mountpoint)
			if err != nil {
				addError("filesystems", fmt.Errorf("mount %q: %w", partition.Mountpoint, err))
				continue
			}
			if usage == nil {
				addError("filesystems", fmt.Errorf("mount %q: usage unavailable", partition.Mountpoint))
				continue
			}
			snapshot.Filesystems = append(snapshot.Filesystems, Filesystem{
				Mountpoint:     partition.Mountpoint,
				Device:         partition.Device,
				FilesystemType: partition.Fstype,
				UsedBytes:      usage.Used,
				TotalBytes:     usage.Total,
				AvailableBytes: usage.Free,
				UsedPercent:    usage.UsedPercent,
			})
		}
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}

	snapshot.Network.Interfaces = []NetworkInterface{}
	interfaces, err := c.functions.networkCounters(ctx)
	if err != nil {
		addError("network", err)
	} else {
		sort.Slice(interfaces, func(i, j int) bool {
			return interfaces[i].Name < interfaces[j].Name
		})
		for _, iface := range interfaces {
			snapshot.Network.BytesReceived += iface.BytesRecv
			snapshot.Network.BytesSent += iface.BytesSent
			snapshot.Network.PacketsReceived += iface.PacketsRecv
			snapshot.Network.PacketsSent += iface.PacketsSent
			snapshot.Network.Interfaces = append(snapshot.Network.Interfaces, NetworkInterface{
				Name:            iface.Name,
				BytesReceived:   iface.BytesRecv,
				BytesSent:       iface.BytesSent,
				PacketsReceived: iface.PacketsRecv,
				PacketsSent:     iface.PacketsSent,
			})
		}
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}

	snapshot.Errors = errs
	return snapshot, nil
}

func filterFilesystemPartitions(partitions []disk.PartitionStat) ([]disk.PartitionStat, bool) {
	filtered := make([]disk.PartitionStat, 0, len(partitions))
	for _, partition := range partitions {
		mountpoint := strings.TrimSpace(partition.Mountpoint)
		if mountpoint == "" || isPseudoFilesystem(partition.Fstype) {
			continue
		}
		partition.Mountpoint = filepath.Clean(mountpoint)
		filtered = append(filtered, partition)
	}

	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Mountpoint != filtered[j].Mountpoint {
			return filtered[i].Mountpoint < filtered[j].Mountpoint
		}
		if filtered[i].Device != filtered[j].Device {
			if filtered[i].Device == "" {
				return false
			}
			if filtered[j].Device == "" {
				return true
			}
			return filtered[i].Device < filtered[j].Device
		}
		return filtered[i].Fstype < filtered[j].Fstype
	})

	unique := filtered[:0]
	seenMountpoints := make(map[string]struct{}, len(filtered))
	for _, partition := range filtered {
		if _, exists := seenMountpoints[partition.Mountpoint]; exists {
			continue
		}
		seenMountpoints[partition.Mountpoint] = struct{}{}
		unique = append(unique, partition)
	}
	truncated := len(unique) > maxFilesystemMounts
	if truncated {
		unique = unique[:maxFilesystemMounts]
	}
	return unique, truncated
}

func isPseudoFilesystem(filesystemType string) bool {
	switch strings.ToLower(strings.TrimSpace(filesystemType)) {
	case "autofs", "binfmt_misc", "bpf", "cgroup", "cgroup2", "configfs", "debugfs", "devfs", "devpts", "devtmpfs", "efivarfs", "fdesc", "fuse.lxcfs", "fusectl", "hugetlbfs", "kernfs", "mqueue", "nsfs", "nullfs", "proc", "procfs", "pstore", "rootfs", "rpc_pipefs", "securityfs", "selinuxfs", "smackfs", "synthfs", "sysfs", "tracefs", "usbfs", "volfs":
		return true
	default:
		return false
	}
}
