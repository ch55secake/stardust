package metrics

import (
	"context"
	"errors"
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
