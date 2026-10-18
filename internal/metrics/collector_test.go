package metrics

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
)

func fixtureCollector(timeout time.Duration, diskPath string) *Collector {
	collector := NewCollector(timeout, diskPath)
	collector.functions = collectorFunctions{
		hostInfo: func(context.Context) (*host.InfoStat, error) {
			return &host.InfoStat{
				Hostname:        "fixture-host",
				OS:              "linux",
				Platform:        "debian",
				PlatformVersion: "12",
				KernelArch:      "x86_64",
				Uptime:          60,
			}, nil
		},
		cpuPercent:        func(context.Context) ([]float64, error) { return []float64{25}, nil },
		cpuPercentPerCore: func(context.Context) ([]float64, error) { return []float64{20, 30, 70, 10}, nil },
		logicalCores: func(context.Context) (int, error) {
			return 4, nil
		},
		loadAverage: func(context.Context) (*load.AvgStat, error) {
			return &load.AvgStat{Load1: 0.25, Load5: 1.5, Load15: 3.2}, nil
		},
		virtualMemory: func(context.Context) (*mem.VirtualMemoryStat, error) {
			return &mem.VirtualMemoryStat{Used: 1000, Total: 2000, UsedPercent: 50}, nil
		},
		swapMemory: func(context.Context) (*mem.SwapMemoryStat, error) {
			return &mem.SwapMemoryStat{Total: 1000, Used: 300, Free: 700, UsedPercent: 30}, nil
		},
		diskUsage: func(_ context.Context, path string) (*disk.UsageStat, error) {
			return &disk.UsageStat{Path: path, Used: 100, Total: 1000, UsedPercent: 10}, nil
		},
		diskPartitions: func(context.Context) ([]disk.PartitionStat, error) {
			return []disk.PartitionStat{}, nil
		},
		networkCounters: func(context.Context) ([]psnet.IOCountersStat, error) {
			return []psnet.IOCountersStat{
				{Name: "lo", BytesRecv: 200, BytesSent: 75, PacketsRecv: 20, PacketsSent: 8},
				{Name: "eth0", BytesRecv: 100, BytesSent: 50, PacketsRecv: 10, PacketsSent: 5},
			}, nil
		},
	}
	return collector
}

func TestCollectorBuildsSnapshotAndAggregatesNetworkCounters(t *testing.T) {
	snapshot, err := fixtureCollector(time.Second, "/fixture").Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snapshot.Hostname != "fixture-host" || snapshot.OS.Name != "linux" || snapshot.Uptime != 60 {
		t.Fatalf("host fields = %+v, want fixture host metadata", snapshot)
	}
	wantCPU := CPU{
		UsagePercent:        25,
		LogicalCores:        4,
		PerCoreUsagePercent: []float64{20, 30, 70, 10},
		LoadAverage:         &LoadAverage{OneMinute: 0.25, FiveMinutes: 1.5, FifteenMinutes: 3.2},
	}
	if !reflect.DeepEqual(snapshot.CPU, wantCPU) {
		t.Fatalf("cpu = %+v, want 25%% across 4 logical cores", snapshot.CPU)
	}
	if len(snapshot.CPU.PerCoreUsagePercent) != snapshot.CPU.LogicalCores {
		t.Fatalf("per-core usage count = %d, want %d", len(snapshot.CPU.PerCoreUsagePercent), snapshot.CPU.LogicalCores)
	}
	for i, percent := range snapshot.CPU.PerCoreUsagePercent {
		if percent < 0 || percent > 100 {
			t.Errorf("per-core usage at index %d = %v, want a percentage in [0, 100]", i, percent)
		}
	}
	if snapshot.Memory != (Memory{UsedBytes: 1000, TotalBytes: 2000, UsedPercent: 50}) {
		t.Fatalf("memory = %+v, want fixture memory", snapshot.Memory)
	}
	if snapshot.Swap != (Swap{TotalBytes: 1000, UsedBytes: 300, FreeBytes: 700, UsedPercent: 30}) {
		t.Fatalf("swap = %+v, want fixture swap", snapshot.Swap)
	}
	if snapshot.Disk != (Disk{Path: "/fixture", UsedBytes: 100, TotalBytes: 1000, UsedPercent: 10}) {
		t.Fatalf("disk = %+v, want fixture disk", snapshot.Disk)
	}
	wantNetwork := Network{
		BytesReceived:   300,
		BytesSent:       125,
		PacketsReceived: 30,
		PacketsSent:     13,
		Interfaces: []NetworkInterface{
			{Name: "eth0", BytesReceived: 100, BytesSent: 50, PacketsReceived: 10, PacketsSent: 5},
			{Name: "lo", BytesReceived: 200, BytesSent: 75, PacketsReceived: 20, PacketsSent: 8},
		},
	}
	if !reflect.DeepEqual(snapshot.Network, wantNetwork) {
		t.Fatalf("network = %+v, want %+v", snapshot.Network, wantNetwork)
	}
	if len(snapshot.Errors) != 0 {
		t.Fatalf("errors = %+v, want none", snapshot.Errors)
	}
}

func TestCollectorReportsPerCoreAndLoadAverageFailuresIndependently(t *testing.T) {
	tests := []struct {
		name        string
		failPerCore bool
		failLoadAvg bool
		wantErrors  []MetricError
	}{
		{
			name:        "per-core unavailable",
			failPerCore: true,
			wantErrors:  []MetricError{{Metric: "cpu", Message: "per-core CPU usage unavailable"}},
		},
		{
			name:        "load average unsupported",
			failLoadAvg: true,
			wantErrors:  []MetricError{{Metric: "load_average", Message: "load averages unsupported"}},
		},
		{
			name:        "both unavailable",
			failPerCore: true,
			failLoadAvg: true,
			wantErrors: []MetricError{
				{Metric: "cpu", Message: "per-core CPU usage unavailable"},
				{Metric: "load_average", Message: "load averages unsupported"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collector := fixtureCollector(time.Second, "/fixture")
			if test.failPerCore {
				collector.functions.cpuPercentPerCore = func(context.Context) ([]float64, error) {
					return nil, errors.New("per-core CPU usage unavailable")
				}
			}
			if test.failLoadAvg {
				collector.functions.loadAverage = func(context.Context) (*load.AvgStat, error) {
					return nil, errors.New("load averages unsupported")
				}
			}

			snapshot, err := collector.Collect(context.Background())
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			if snapshot.CPU.UsagePercent != 25 || snapshot.CPU.LogicalCores != 4 {
				t.Fatalf("aggregate CPU metrics = %+v, want available values despite optional metric failures", snapshot.CPU)
			}
			if test.failPerCore {
				if snapshot.CPU.PerCoreUsagePercent == nil || len(snapshot.CPU.PerCoreUsagePercent) != 0 {
					t.Errorf("per-core usage = %v, want an empty array when unavailable", snapshot.CPU.PerCoreUsagePercent)
				}
			} else if !reflect.DeepEqual(snapshot.CPU.PerCoreUsagePercent, []float64{20, 30, 70, 10}) {
				t.Errorf("per-core usage = %v, want fixture values", snapshot.CPU.PerCoreUsagePercent)
			}
			if test.failLoadAvg {
				if snapshot.CPU.LoadAverage != nil {
					t.Errorf("load average = %+v, want omitted when unavailable", snapshot.CPU.LoadAverage)
				}
			} else if !reflect.DeepEqual(snapshot.CPU.LoadAverage, &LoadAverage{OneMinute: 0.25, FiveMinutes: 1.5, FifteenMinutes: 3.2}) {
				t.Errorf("load average = %+v, want fixture values", snapshot.CPU.LoadAverage)
			}
			if !reflect.DeepEqual(snapshot.Errors, test.wantErrors) {
				t.Errorf("errors = %+v, want %+v", snapshot.Errors, test.wantErrors)
			}
		})
	}
}

func TestCollectorReportsNetworkFailureAsPartialMetric(t *testing.T) {
	collector := fixtureCollector(time.Second, "/fixture")
	collector.functions.networkCounters = func(context.Context) ([]psnet.IOCountersStat, error) {
		return nil, errors.New("network counters unavailable")
	}

	snapshot, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snapshot.CPU.LogicalCores != 4 || snapshot.Memory.TotalBytes != 2000 {
		t.Fatalf("other metrics missing from partial snapshot: %+v", snapshot)
	}
	if snapshot.Network.Interfaces == nil || len(snapshot.Network.Interfaces) != 0 {
		t.Fatalf("network interfaces = %+v, want an empty array", snapshot.Network.Interfaces)
	}
	if len(snapshot.Errors) != 1 || snapshot.Errors[0] != (MetricError{Metric: "network", Message: "network counters unavailable"}) {
		t.Fatalf("errors = %+v, want one network metric error", snapshot.Errors)
	}
}

func TestCollectorReportsFilesystemsAndKeepsConfiguredDiskMetric(t *testing.T) {
	collector := fixtureCollector(time.Second, "/fixture")
	collector.functions.diskPartitions = func(context.Context) ([]disk.PartitionStat, error) {
		return []disk.PartitionStat{
			{Device: "/dev/z-data", Mountpoint: "/data/", Fstype: "ext4"},
			{Device: "/dev/root", Mountpoint: "/", Fstype: "apfs"},
			{Device: "/dev/a-data", Mountpoint: "/data", Fstype: "xfs"},
			{Device: "proc", Mountpoint: "/proc", Fstype: "proc"},
			{Device: "sysfs", Mountpoint: "/sys", Fstype: "sysfs"},
			{Device: "/dev/disk2", Mountpoint: "/run", Fstype: "tmpfs"},
			{Device: "/dev/empty", Mountpoint: "", Fstype: "ext4"},
		}, nil
	}
	var usagePaths []string
	collector.functions.diskUsage = func(_ context.Context, path string) (*disk.UsageStat, error) {
		usagePaths = append(usagePaths, path)
		switch path {
		case "/fixture":
			return &disk.UsageStat{Path: path, Used: 100, Total: 1000, UsedPercent: 10}, nil
		case "/":
			return &disk.UsageStat{Path: path, Used: 700, Total: 1000, Free: 300, UsedPercent: 70}, nil
		case "/data":
			return &disk.UsageStat{Path: path, Used: 400, Total: 2000, Free: 1600, UsedPercent: 20}, nil
		case "/run":
			return &disk.UsageStat{Path: path, Used: 50, Total: 100, Free: 50, UsedPercent: 50}, nil
		default:
			return nil, errors.New("unexpected mount point")
		}
	}

	snapshot, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snapshot.Disk != (Disk{Path: "/fixture", UsedBytes: 100, TotalBytes: 1000, UsedPercent: 10}) {
		t.Fatalf("disk = %+v, want unchanged configured-path metric", snapshot.Disk)
	}
	wantFilesystems := []Filesystem{
		{Mountpoint: "/", Device: "/dev/root", FilesystemType: "apfs", UsedBytes: 700, TotalBytes: 1000, AvailableBytes: 300, UsedPercent: 70},
		{Mountpoint: "/data", Device: "/dev/a-data", FilesystemType: "xfs", UsedBytes: 400, TotalBytes: 2000, AvailableBytes: 1600, UsedPercent: 20},
		{Mountpoint: "/run", Device: "/dev/disk2", FilesystemType: "tmpfs", UsedBytes: 50, TotalBytes: 100, AvailableBytes: 50, UsedPercent: 50},
	}
	if !reflect.DeepEqual(snapshot.Filesystems, wantFilesystems) {
		t.Fatalf("filesystems = %+v, want %+v", snapshot.Filesystems, wantFilesystems)
	}
	if !reflect.DeepEqual(usagePaths, []string{"/fixture", "/", "/data", "/run"}) {
		t.Fatalf("usage paths = %v, want configured path and unique non-pseudo mounts", usagePaths)
	}
	if len(snapshot.Errors) != 0 {
		t.Fatalf("errors = %+v, want none", snapshot.Errors)
	}
}

func TestCollectorKeepsAccessibleFilesystemsWhenMountUsageFails(t *testing.T) {
	collector := fixtureCollector(time.Second, "/fixture")
	collector.functions.diskPartitions = func(context.Context) ([]disk.PartitionStat, error) {
		return []disk.PartitionStat{
			{Device: "/dev/root", Mountpoint: "/", Fstype: "ext4"},
			{Device: "//server/share", Mountpoint: "/private", Fstype: "smbfs"},
		}, nil
	}
	collector.functions.diskUsage = func(_ context.Context, path string) (*disk.UsageStat, error) {
		if path == "/private" {
			return nil, errors.New("permission denied")
		}
		return &disk.UsageStat{Path: path, Used: 100, Total: 500, Free: 400, UsedPercent: 20}, nil
	}

	snapshot, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(snapshot.Filesystems) != 1 || snapshot.Filesystems[0].Mountpoint != "/" {
		t.Fatalf("filesystems = %+v, want the accessible root mount", snapshot.Filesystems)
	}
	if snapshot.Disk.Path != "/fixture" {
		t.Fatalf("disk path = %q, want configured path despite another mount failure", snapshot.Disk.Path)
	}
	wantError := MetricError{Metric: "filesystems", Message: `mount "/private": permission denied`}
	if !reflect.DeepEqual(snapshot.Errors, []MetricError{wantError}) {
		t.Fatalf("errors = %+v, want %+v", snapshot.Errors, wantError)
	}
}

func TestCollectorReportsFilesystemEnumerationFailureAsPartialMetric(t *testing.T) {
	collector := fixtureCollector(time.Second, "/fixture")
	collector.functions.diskPartitions = func(context.Context) ([]disk.PartitionStat, error) {
		return nil, errors.New("mount table unavailable")
	}

	snapshot, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snapshot.Filesystems == nil || len(snapshot.Filesystems) != 0 {
		t.Fatalf("filesystems = %+v, want an empty array", snapshot.Filesystems)
	}
	if snapshot.Disk.Path != "/fixture" || snapshot.Memory.TotalBytes != 2000 {
		t.Fatalf("unrelated metrics missing after filesystem enumeration failure: %+v", snapshot)
	}
	wantErrors := []MetricError{{Metric: "filesystems", Message: "mount table unavailable"}}
	if !reflect.DeepEqual(snapshot.Errors, wantErrors) {
		t.Fatalf("errors = %+v, want %+v", snapshot.Errors, wantErrors)
	}
}

func TestFilterFilesystemPartitionsDeduplicatesAndCaps(t *testing.T) {
	partitions := []disk.PartitionStat{
		{Device: "/dev/z", Mountpoint: "/data/", Fstype: "ext4"},
		{Device: "/dev/b", Mountpoint: "/data", Fstype: "ext4"},
		{Device: "/dev/a", Mountpoint: "/data", Fstype: "xfs"},
		{Device: "proc", Mountpoint: "/proc", Fstype: "proc"},
		{Device: "devfs", Mountpoint: "/dev", Fstype: "devfs"},
		{Device: "hugetlbfs", Mountpoint: "/huge", Fstype: "hugetlbfs"},
		{Device: "disk", Mountpoint: "/alias", Fstype: "nullfs"},
		{Device: "/dev/empty", Mountpoint: "  ", Fstype: "ext4"},
	}
	filtered, truncated := filterFilesystemPartitions(partitions)
	if truncated {
		t.Fatal("unexpected truncation")
	}
	want := []disk.PartitionStat{{Device: "/dev/a", Mountpoint: "/data", Fstype: "xfs"}}
	if !reflect.DeepEqual(filtered, want) {
		t.Fatalf("filtered partitions = %+v, want %+v", filtered, want)
	}

	partitions = make([]disk.PartitionStat, maxFilesystemMounts+1)
	for i := range partitions {
		partitions[i] = disk.PartitionStat{Device: "/dev/test", Mountpoint: fmt.Sprintf("/volume-%03d", i), Fstype: "ext4"}
	}
	filtered, truncated = filterFilesystemPartitions(partitions)
	if !truncated || len(filtered) != maxFilesystemMounts {
		t.Fatalf("filtered count = %d, truncated = %t, want cap %d and truncation", len(filtered), truncated, maxFilesystemMounts)
	}
	if filtered[0].Mountpoint != "/volume-000" || filtered[len(filtered)-1].Mountpoint != "/volume-063" {
		t.Fatalf("capped mounts range from %q to %q, want first 64 sorted mounts", filtered[0].Mountpoint, filtered[len(filtered)-1].Mountpoint)
	}
}

func TestCollectorHandlesHostWithoutSwap(t *testing.T) {
	collector := fixtureCollector(time.Second, "/fixture")
	collector.functions.swapMemory = func(context.Context) (*mem.SwapMemoryStat, error) {
		return &mem.SwapMemoryStat{}, nil
	}

	snapshot, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snapshot.Swap != (Swap{}) {
		t.Fatalf("swap = %+v, want zero values when swap is not configured", snapshot.Swap)
	}
	if len(snapshot.Errors) != 0 {
		t.Fatalf("errors = %+v, want none for a host without swap", snapshot.Errors)
	}
}

func TestCollectorReportsSwapMetricError(t *testing.T) {
	collector := fixtureCollector(time.Second, "/fixture")
	collector.functions.swapMemory = func(context.Context) (*mem.SwapMemoryStat, error) {
		return nil, errors.New("swap metrics unavailable")
	}

	snapshot, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snapshot.Swap != (Swap{}) {
		t.Fatalf("swap = %+v, want zero values when unavailable", snapshot.Swap)
	}
	if len(snapshot.Errors) != 1 || snapshot.Errors[0] != (MetricError{Metric: "swap", Message: "swap metrics unavailable"}) {
		t.Fatalf("errors = %+v, want a swap metric error", snapshot.Errors)
	}
}

func TestCollectorReturnsPartialSnapshotOnMetricFailure(t *testing.T) {
	collector := fixtureCollector(time.Second, "/fixture")
	collector.functions.hostInfo = func(context.Context) (*host.InfoStat, error) {
		return nil, errors.New("host information unavailable")
	}

	snapshot, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snapshot.CPU.LogicalCores != 4 || snapshot.Disk.Path != "/fixture" {
		t.Fatalf("available data missing from partial snapshot: %+v", snapshot)
	}
	if len(snapshot.Errors) != 1 || snapshot.Errors[0].Metric != "host" {
		t.Fatalf("errors = %+v, want one host error", snapshot.Errors)
	}
}

func TestCollectorStopsAfterCollectionDeadline(t *testing.T) {
	collector := fixtureCollector(20*time.Millisecond, "/fixture")
	cpuCalled := false
	collector.functions.hostInfo = func(ctx context.Context) (*host.InfoStat, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	collector.functions.cpuPercent = func(context.Context) ([]float64, error) {
		cpuCalled = true
		return nil, nil
	}

	started := time.Now()
	_, err := collector.Collect(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("collect error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("collection exceeded deadline by too much: %s", elapsed)
	}
	if cpuCalled {
		t.Fatal("collector continued to CPU sampling after deadline")
	}
}

func TestCollectorHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixtureCollector(time.Second, "/fixture").Collect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("collect error = %v, want context canceled", err)
	}
}
