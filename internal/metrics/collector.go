package metrics

import (
	"context"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
)

// Snapshot is the stable metrics payload exposed by the v1 API. Byte fields
// are bytes, percentages are in the inclusive range 0-100, and uptime is in
// seconds.
type Snapshot struct {
	Hostname string        `json:"hostname"`
	OS       OSInfo        `json:"os"`
	CPU      CPU           `json:"cpu"`
	Memory   Memory        `json:"memory"`
	Disk     Disk          `json:"disk"`
	Uptime   uint64        `json:"uptime_seconds"`
	Network  Network       `json:"network"`
	Errors   []MetricError `json:"errors,omitempty"`
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
	UsagePercent float64 `json:"usage_percent"`
	LogicalCores int     `json:"logical_cores"`
}

type Memory struct {
	UsedBytes   uint64  `json:"used_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

type Disk struct {
	Path        string  `json:"path"`
	UsedBytes   uint64  `json:"used_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

type Network struct {
	BytesReceived   uint64 `json:"bytes_received"`
	BytesSent       uint64 `json:"bytes_sent"`
	PacketsReceived uint64 `json:"packets_received"`
	PacketsSent     uint64 `json:"packets_sent"`
}

type Collector struct {
	timeout  time.Duration
	diskPath string
}

func NewCollector(timeout time.Duration, diskPath string) *Collector {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if diskPath == "" {
		diskPath = "/"
	}
	return &Collector{timeout: timeout, diskPath: diskPath}
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

	info, err := host.InfoWithContext(ctx)
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

	percent, err := cpu.PercentWithContext(ctx, 0, false)
	if err != nil {
		addError("cpu", err)
	} else if len(percent) > 0 {
		snapshot.CPU.UsagePercent = percent[0]
	}
	cores, err := cpu.CountsWithContext(ctx, true)
	if err != nil {
		addError("cpu", err)
	} else {
		snapshot.CPU.LogicalCores = cores
	}

	memory, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		addError("memory", err)
	} else {
		snapshot.Memory = Memory{
			UsedBytes:   memory.Used,
			TotalBytes:  memory.Total,
			UsedPercent: memory.UsedPercent,
		}
	}

	usage, err := disk.UsageWithContext(ctx, c.diskPath)
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

	interfaces, err := psnet.IOCountersWithContext(ctx, false)
	if err != nil {
		addError("network", err)
	} else {
		for _, iface := range interfaces {
			snapshot.Network.BytesReceived += iface.BytesRecv
			snapshot.Network.BytesSent += iface.BytesSent
			snapshot.Network.PacketsReceived += iface.PacketsRecv
			snapshot.Network.PacketsSent += iface.PacketsSent
		}
	}

	snapshot.Errors = errs
	return snapshot, nil
}
