package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
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
		cpuPercent: func(context.Context) ([]float64, error) { return []float64{25}, nil },
		logicalCores: func(context.Context) (int, error) {
			return 4, nil
		},
		virtualMemory: func(context.Context) (*mem.VirtualMemoryStat, error) {
			return &mem.VirtualMemoryStat{Used: 1000, Total: 2000, UsedPercent: 50}, nil
		},
		diskUsage: func(_ context.Context, path string) (*disk.UsageStat, error) {
			return &disk.UsageStat{Path: path, Used: 100, Total: 1000, UsedPercent: 10}, nil
		},
		networkCounters: func(context.Context) ([]psnet.IOCountersStat, error) {
			return []psnet.IOCountersStat{
				{BytesRecv: 100, BytesSent: 50, PacketsRecv: 10, PacketsSent: 5},
				{BytesRecv: 200, BytesSent: 75, PacketsRecv: 20, PacketsSent: 8},
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
	if snapshot.CPU.UsagePercent != 25 || snapshot.CPU.LogicalCores != 4 {
		t.Fatalf("cpu = %+v, want 25%% across 4 logical cores", snapshot.CPU)
	}
	if snapshot.Memory != (Memory{UsedBytes: 1000, TotalBytes: 2000, UsedPercent: 50}) {
		t.Fatalf("memory = %+v, want fixture memory", snapshot.Memory)
	}
	if snapshot.Disk != (Disk{Path: "/fixture", UsedBytes: 100, TotalBytes: 1000, UsedPercent: 10}) {
		t.Fatalf("disk = %+v, want fixture disk", snapshot.Disk)
	}
	wantNetwork := Network{BytesReceived: 300, BytesSent: 125, PacketsReceived: 30, PacketsSent: 13}
	if snapshot.Network != wantNetwork {
		t.Fatalf("network = %+v, want %+v", snapshot.Network, wantNetwork)
	}
	if len(snapshot.Errors) != 0 {
		t.Fatalf("errors = %+v, want none", snapshot.Errors)
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
