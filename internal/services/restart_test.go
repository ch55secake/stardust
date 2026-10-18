package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testRestarter struct {
	supported bool
	calls     atomic.Int32
	restart   func(context.Context, string) error
}

func (r *testRestarter) Supported() bool { return r.supported }
func (r *testRestarter) Restart(ctx context.Context, unit string) error {
	r.calls.Add(1)
	if r.restart != nil {
		return r.restart(ctx, unit)
	}
	return nil
}

type testInspector struct {
	supported bool
	inspect   func(context.Context, string) (Unit, error)
}

func (i *testInspector) Supported() bool { return i.supported }
func (i *testInspector) Inspect(ctx context.Context, unit string) (Unit, error) {
	if i.inspect != nil {
		return i.inspect(ctx, unit)
	}
	return Unit{Name: unit, ActiveState: "active", SubState: "running"}, nil
}

func newTestRestartManager(t *testing.T, path string, restarter Restarter, inspector Inspector, output *bytes.Buffer) *RestartManager {
	t.Helper()
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(output, nil))
	policy, err := NewPolicyWithRestart(testToken, []string{"worker.service", "other.service"}, strings.Repeat("r", 32), []string{"worker.service", "other.service"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewRestartManager(path, policy, restarter, inspector, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager
}

func waitRestartOperation(t *testing.T, manager *RestartManager, id string) RestartOperation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		operation, err := manager.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if operation.State != RestartRunning {
			return operation
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("restart operation did not complete")
	return RestartOperation{}
}

func TestRestartManagerSuccessDurableIdempotencyAndAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.json")
	var logs bytes.Buffer
	key := strings.Repeat("k", 48)
	restarter := &testRestarter{supported: true}
	manager := newTestRestartManager(t, path, restarter, &testInspector{supported: true}, &logs)
	operation, created, err := manager.Start("worker.service", key, "198.51.100.8:1234\r\nforged")
	if err != nil || !created || operation.ID == "" || operation.State != RestartRunning {
		t.Fatalf("Start = %+v, created=%v, err=%v", operation, created, err)
	}
	completed := waitRestartOperation(t, manager, operation.ID)
	if completed.State != RestartSucceeded || completed.JobResult != "succeeded" || completed.ActiveState != "active" || completed.SubState != "running" {
		t.Fatalf("unexpected result: %+v", completed)
	}
	duplicate, created, err := manager.Start("worker.service", key, "198.51.100.9:4321\r\nforged-retry")
	if err != nil || created || duplicate.ID != operation.ID || restarter.calls.Load() != 1 {
		t.Fatalf("idempotent retry = %+v, created=%v, calls=%d, err=%v", duplicate, created, restarter.calls.Load(), err)
	}
	if _, _, err := manager.Start("other.service", key, "198.51.100.9:4321"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("reused key for another unit: %v", err)
	}
	if strings.Contains(logs.String(), key) || strings.Contains(logs.String(), "\nforged") || !strings.Contains(logs.String(), "operation_id") {
		t.Fatalf("unsafe or incomplete audit output: %q", logs.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), key) || !strings.Contains(string(data), "idempotency_hash") || !strings.Contains(string(data), "198.51.100.8:1234��forged") {
		t.Fatalf("unexpected persisted audit record: %s", data)
	}
	var stored restartStore
	if err := json.Unmarshal(data, &stored); err != nil || len(stored.Records) != 1 {
		t.Fatalf("invalid operation store: %+v (%v)", stored, err)
	}
	manager.Close()

	reloaded := newTestRestartManager(t, path, restarter, &testInspector{supported: true}, &logs)
	duplicate, created, err = reloaded.Start("worker.service", key, "retry")
	if err != nil || created || duplicate.ID != operation.ID || restarter.calls.Load() != 1 {
		t.Fatalf("durable retry = %+v, created=%v, calls=%d, err=%v", duplicate, created, restarter.calls.Load(), err)
	}
}

func TestRestartManagerRejectsConcurrentDifferentRequestsAndReplaysSameKey(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	restarter := &testRestarter{supported: true, restart: func(ctx context.Context, _ string) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	manager := newTestRestartManager(t, filepath.Join(t.TempDir(), "operations.json"), restarter, &testInspector{supported: true}, &bytes.Buffer{})
	key := strings.Repeat("x", 32)
	operation, created, err := manager.Start("worker.service", key, "client")
	if err != nil || !created {
		t.Fatalf("Start = %+v, %v", operation, err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("restart backend was not called")
	}
	duplicate, created, err := manager.Start("worker.service", key, "retry")
	if err != nil || created || duplicate.ID != operation.ID {
		t.Fatalf("concurrent duplicate = %+v, created=%v, err=%v", duplicate, created, err)
	}
	if _, _, err := manager.Start("worker.service", strings.Repeat("y", 32), "second client"); !errors.Is(err, ErrUnitBusy) {
		t.Fatalf("parallel restart was not rejected: %v", err)
	}
	close(release)
	if got := waitRestartOperation(t, manager, operation.ID); got.State != RestartSucceeded {
		t.Fatalf("operation did not complete: %+v", got)
	}
	if restarter.calls.Load() != 1 {
		t.Fatalf("backend called %d times", restarter.calls.Load())
	}
}

func TestRestartManagerEnforcesItsOwnAllowlist(t *testing.T) {
	restarter := &testRestarter{supported: true}
	manager := newTestRestartManager(t, filepath.Join(t.TempDir(), "operations.json"), restarter, &testInspector{supported: true}, &bytes.Buffer{})
	if _, _, err := manager.Start("denied.service", strings.Repeat("d", 32), "client"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager accepted a unit outside its restart policy: %v", err)
	}
	if restarter.calls.Load() != 0 {
		t.Fatal("disallowed unit reached the restart backend")
	}
}

func TestRestartManagerReportsFailureTimeoutAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name  string
		run   func(context.Context, string) error
		state string
		job   string
	}{
		{"job failure", func(context.Context, string) error { return ErrRestartFailed }, RestartFailed, "failed"},
		{"timeout", func(context.Context, string) error { return context.DeadlineExceeded }, RestartTimedOut, "unknown"},
		{"cancellation", func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }, RestartCancelled, "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			restarter := &testRestarter{supported: true, restart: test.run}
			manager := newTestRestartManager(t, filepath.Join(t.TempDir(), "operations.json"), restarter, &testInspector{supported: true}, &bytes.Buffer{})
			operation, created, err := manager.Start("worker.service", strings.Repeat("a", 32), "client")
			if err != nil || !created {
				t.Fatalf("Start = %+v, %v", operation, err)
			}
			if test.name == "cancellation" {
				manager.Close()
			}
			got := waitRestartOperation(t, manager, operation.ID)
			if got.State != test.state || got.JobResult != test.job || got.CompletedAt == nil {
				t.Fatalf("operation = %+v, want state=%s job=%s", got, test.state, test.job)
			}
		})
	}
}

func TestRestartManagerInterruptedRecoveryAndBoundedPolling(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "operations.json")
	key := strings.Repeat("z", 32)
	hash := sha256.Sum256([]byte(key))
	operation := RestartOperation{ID: strings.Repeat("1", 32), Unit: "worker.service", State: RestartRunning, RequestedAt: time.Now().UTC()}
	store := restartStore{Version: restartStoreVersion, Records: []restartRecord{{Operation: operation, IdempotencyHash: hex.EncodeToString(hash[:]), Caller: "client"}}}
	data, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	restarter := &testRestarter{supported: true}
	manager := newTestRestartManager(t, path, restarter, &testInspector{supported: true}, &bytes.Buffer{})
	got, err := manager.Status(operation.ID)
	if err != nil || got.State != RestartInterrupted || got.ErrorCode != "interrupted" {
		t.Fatalf("recovered status = %+v, err=%v", got, err)
	}
	if _, created, err := manager.Start(operation.Unit, key, "retry"); err != nil || created || restarter.calls.Load() != 0 {
		t.Fatalf("interrupted operation replayed: created=%v calls=%d err=%v", created, restarter.calls.Load(), err)
	}

	old := time.Now().UTC().Add(-RestartOperationTTL - time.Minute)
	manager.mu.Lock()
	record := manager.operations[operation.ID]
	record.Operation.RequestedAt = old
	manager.operations[operation.ID] = record
	if err := manager.persistLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()
	if _, err := manager.Status(operation.ID); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("expired operation remains pollable: %v", err)
	}
}

func TestRestartManagerValidatesStorePermissionsAndUnits(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "operations.json")
	policy, err := NewPolicyWithRestart(testToken, []string{"worker.service"}, strings.Repeat("r", 32), []string{"worker.service"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewRestartManager(path, policy, &testRestarter{supported: true}, &testInspector{supported: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.Close()
	for _, unit := range []string{"", "--help.service", "worker@a\\x2db.service", "bad;id.service"} {
		if ValidRestartUnit(unit) {
			t.Errorf("unsafe restart unit accepted: %q", unit)
		}
	}
	if !ValidRestartUnit("worker@blue.service") {
		t.Fatal("valid exact restart unit rejected")
	}
}
