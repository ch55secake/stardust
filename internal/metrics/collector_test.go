package metrics

import (
	"context"
	"testing"
	"time"
)

func TestCollectorReturnsSystemSnapshot(t *testing.T) {
	snapshot, err := NewCollector(5*time.Second, "/").Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snapshot.Disk.Path != "/" {
		t.Fatalf("disk path = %q, want /", snapshot.Disk.Path)
	}
	if snapshot.CPU.LogicalCores < 1 {
		t.Fatalf("logical cores = %d, want at least 1", snapshot.CPU.LogicalCores)
	}
}

func TestCollectorHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewCollector(time.Second, "/").Collect(ctx); err == nil {
		t.Fatal("expected cancelled collection to fail")
	}
}
