//go:build linux

package metrics

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestCollectorReadsLinuxHostMetrics(t *testing.T) {
	diskPath := t.TempDir()
	snapshot, err := NewCollector(5*time.Second, diskPath).Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	wantHostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("get hostname: %v", err)
	}
	if snapshot.Hostname != wantHostname {
		t.Errorf("hostname = %q, want %q", snapshot.Hostname, wantHostname)
	}
	if snapshot.OS.Name != "linux" {
		t.Errorf("OS name = %q, want linux", snapshot.OS.Name)
	}
	if snapshot.OS.Platform == "" || snapshot.OS.KernelArch == "" {
		t.Errorf("OS metadata is incomplete: %+v", snapshot.OS)
	}
	if snapshot.Uptime == 0 {
		t.Error("uptime is zero, want positive seconds")
	}
	if snapshot.CPU.LogicalCores < 1 {
		t.Errorf("logical cores = %d, want at least 1", snapshot.CPU.LogicalCores)
	}
	if snapshot.CPU.UsagePercent < 0 || snapshot.CPU.UsagePercent > 100 {
		t.Errorf("CPU usage = %f, want 0-100 percent", snapshot.CPU.UsagePercent)
	}
	if snapshot.Memory.TotalBytes == 0 || snapshot.Memory.UsedBytes > snapshot.Memory.TotalBytes {
		t.Errorf("memory = %+v, want used and total bytes within bounds", snapshot.Memory)
	}
	if snapshot.Memory.UsedPercent < 0 || snapshot.Memory.UsedPercent > 100 {
		t.Errorf("memory usage = %f, want 0-100 percent", snapshot.Memory.UsedPercent)
	}
	if snapshot.Disk.Path != diskPath {
		t.Errorf("disk path = %q, want configured path %q", snapshot.Disk.Path, diskPath)
	}
	if snapshot.Disk.TotalBytes == 0 || snapshot.Disk.UsedBytes > snapshot.Disk.TotalBytes {
		t.Errorf("disk = %+v, want used and total bytes within bounds", snapshot.Disk)
	}
	if snapshot.Disk.UsedPercent < 0 || snapshot.Disk.UsedPercent > 100 {
		t.Errorf("disk usage = %f, want 0-100 percent", snapshot.Disk.UsedPercent)
	}
}
