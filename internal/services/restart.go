package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	DefaultRestartStorePath = "/var/lib/stardust/restart-operations.json"
	RestartTimeout          = 30 * time.Second
	RestartOperationTTL     = 24 * time.Hour
	MaxRestartOperations    = 1024
	maxRestartStoreBytes    = 1024 * 1024
	restartStoreVersion     = 1
)

const (
	RestartRunning     = "running"
	RestartSucceeded   = "succeeded"
	RestartFailed      = "failed"
	RestartTimedOut    = "timed_out"
	RestartCancelled   = "cancelled"
	RestartInterrupted = "interrupted"
)

type RestartOperation struct {
	ID               string     `json:"operation_id"`
	Unit             string     `json:"unit"`
	State            string     `json:"state"`
	RequestedAt      time.Time  `json:"requested_at"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	JobResult        string     `json:"job_result,omitempty"`
	ActiveState      string     `json:"active_state,omitempty"`
	SubState         string     `json:"sub_state,omitempty"`
	ObservationError string     `json:"observation_error,omitempty"`
	ErrorCode        string     `json:"error_code,omitempty"`
}

type restartRecord struct {
	Operation       RestartOperation `json:"operation"`
	IdempotencyHash string           `json:"idempotency_hash"`
	Caller          string           `json:"caller"`
}

type restartStore struct {
	Version int             `json:"version"`
	Records []restartRecord `json:"records"`
}

// RestartManager durably deduplicates requests before starting any systemd job.
// If the process exits while a job is in progress, that operation becomes
// interrupted on startup and is never replayed automatically.
type RestartManager struct {
	mu          sync.Mutex
	path        string
	policy      *Policy
	restarter   Restarter
	inspector   Inspector
	logger      *slog.Logger
	ctx         context.Context
	cancel      context.CancelFunc
	operations  map[string]restartRecord
	keys        map[string]string
	activeUnits map[string]string
}

func ValidIdempotencyKey(key string) bool {
	if len(key) < 32 || len(key) > 128 {
		return false
	}
	for _, character := range key {
		if character < 33 || character > 126 {
			return false
		}
	}
	return true
}

func NewRestartManager(path string, policy *Policy, restarter Restarter, inspector Inspector, logger *slog.Logger) (*RestartManager, error) {
	if policy == nil || !policy.RestartEnabled() {
		return nil, ErrRestartDisabled
	}
	if path == "" || restarter == nil || inspector == nil {
		return nil, ErrOperationStore
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := validateRestartStoreDirectory(filepath.Dir(path)); err != nil {
		return nil, ErrOperationStore
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &RestartManager{
		path:        path,
		policy:      policy,
		restarter:   restarter,
		inspector:   inspector,
		logger:      logger,
		ctx:         ctx,
		cancel:      cancel,
		operations:  make(map[string]restartRecord),
		keys:        make(map[string]string),
		activeUnits: make(map[string]string),
	}
	if err := manager.load(); err != nil {
		cancel()
		return nil, ErrOperationStore
	}
	return manager, nil
}

func validateRestartStoreDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o700 != 0o700 {
		return ErrOperationStore
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrOperationStore
	}
	return nil
}

func (m *RestartManager) load() error {
	linkInfo, err := os.Lstat(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !linkInfo.Mode().IsRegular() || linkInfo.Mode()&os.ModeSymlink != 0 || linkInfo.Mode().Perm() != 0o600 || linkInfo.Size() > maxRestartStoreBytes {
		return ErrOperationStore
	}
	file, err := os.Open(m.path)
	if err != nil {
		return ErrOperationStore
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !os.SameFile(linkInfo, info) {
		return ErrOperationStore
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrOperationStore
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxRestartStoreBytes+1))
	decoder.DisallowUnknownFields()
	var store restartStore
	if err := decoder.Decode(&store); err != nil || store.Version != restartStoreVersion || len(store.Records) > MaxRestartOperations {
		return ErrOperationStore
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrOperationStore
	}
	now := time.Now().UTC()
	changed := false
	for _, record := range store.Records {
		operation := record.Operation
		if !validOperationID(operation.ID) || !ValidRestartUnit(operation.Unit) || !validIdempotencyHash(record.IdempotencyHash) || operation.RequestedAt.IsZero() || operation.RequestedAt.After(now.Add(5*time.Minute)) || !validRestartState(operation.State) {
			return ErrOperationStore
		}
		if _, exists := m.operations[operation.ID]; exists {
			return ErrOperationStore
		}
		if _, exists := m.keys[record.IdempotencyHash]; exists {
			return ErrOperationStore
		}
		if operation.State == RestartRunning {
			operation.State = RestartInterrupted
			operation.ErrorCode = "interrupted"
			completedAt := now
			operation.CompletedAt = &completedAt
			record.Operation = operation
			changed = true
		}
		if now.Sub(operation.RequestedAt) > RestartOperationTTL && operation.State != RestartRunning {
			changed = true
			continue
		}
		m.operations[operation.ID] = record
		m.keys[record.IdempotencyHash] = operation.ID
		if operation.State == RestartRunning {
			m.activeUnits[operation.Unit] = operation.ID
		}
	}
	if changed {
		return m.persistLocked()
	}
	return nil
}

func (m *RestartManager) Start(unit, key, caller string) (RestartOperation, bool, error) {
	if !ValidRestartUnit(unit) {
		return RestartOperation{}, false, ErrForbidden
	}
	if !m.policy.AllowsRestart(unit) {
		return RestartOperation{}, false, ErrForbidden
	}
	if !ValidIdempotencyKey(key) {
		return RestartOperation{}, false, errors.New("invalid idempotency key")
	}
	caller = sanitizeAuditCaller(caller)
	if !m.restarter.Supported() {
		return RestartOperation{}, false, ErrUnsupported
	}
	keyHash := sha256.Sum256([]byte(key))
	keyDigest := hex.EncodeToString(keyHash[:])
	now := time.Now().UTC()

	m.mu.Lock()
	m.pruneLocked(now)
	if existingID, exists := m.keys[keyDigest]; exists {
		existing := m.operations[existingID]
		if existing.Operation.Unit != unit {
			m.mu.Unlock()
			return RestartOperation{}, false, ErrIdempotencyConflict
		}
		operation := existing.Operation
		m.mu.Unlock()
		m.logger.Info("service restart idempotent replay", "operation_id", operation.ID, "unit", operation.Unit, "caller", caller)
		return operation, false, nil
	}
	if _, busy := m.activeUnits[unit]; busy {
		m.mu.Unlock()
		return RestartOperation{}, false, ErrUnitBusy
	}
	if len(m.operations) >= MaxRestartOperations {
		m.mu.Unlock()
		return RestartOperation{}, false, ErrOperationLimit
	}
	operationID, err := newOperationID()
	if err != nil {
		m.mu.Unlock()
		return RestartOperation{}, false, ErrOperationStore
	}
	operation := RestartOperation{ID: operationID, Unit: unit, State: RestartRunning, RequestedAt: now}
	record := restartRecord{Operation: operation, IdempotencyHash: keyDigest, Caller: caller}
	m.operations[operation.ID] = record
	m.keys[keyDigest] = operation.ID
	m.activeUnits[unit] = operation.ID
	if err := m.persistLocked(); err != nil {
		delete(m.operations, operation.ID)
		delete(m.keys, keyDigest)
		delete(m.activeUnits, unit)
		m.mu.Unlock()
		return RestartOperation{}, false, ErrOperationStore
	}
	m.mu.Unlock()

	m.logger.Info("service restart operation accepted", "operation_id", operation.ID, "unit", unit, "caller", caller)
	go m.execute(operation.ID)
	return operation, true, nil
}

func (m *RestartManager) Status(operationID string) (RestartOperation, error) {
	if !validOperationID(operationID) {
		return RestartOperation{}, ErrOperationNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(time.Now().UTC())
	record, exists := m.operations[operationID]
	if !exists {
		return RestartOperation{}, ErrOperationNotFound
	}
	return record.Operation, nil
}

func (m *RestartManager) Close() { m.cancel() }

func (m *RestartManager) execute(operationID string) {
	m.mu.Lock()
	record, exists := m.operations[operationID]
	m.mu.Unlock()
	if !exists {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, RestartTimeout)
	err := m.restarter.Restart(ctx, record.Operation.Unit)
	operation := record.Operation
	switch {
	case err == nil:
		operation.State = RestartSucceeded
		operation.JobResult = "succeeded"
		unit, inspectErr := m.inspector.Inspect(ctx, operation.Unit)
		if inspectErr == nil {
			inspectErr = ctx.Err()
		}
		if inspectErr == nil {
			operation.ActiveState = unit.ActiveState
			operation.SubState = unit.SubState
		} else {
			operation.ObservationError = restartErrorCode(inspectErr)
		}
	case errors.Is(err, context.DeadlineExceeded):
		operation.State = RestartTimedOut
		operation.JobResult = "unknown"
		operation.ErrorCode = "timeout"
	case errors.Is(err, context.Canceled):
		operation.State = RestartCancelled
		operation.JobResult = "unknown"
		operation.ErrorCode = "cancelled"
	default:
		operation.ErrorCode = restartErrorCode(err)
		if errors.Is(err, ErrRestartFailed) {
			operation.State = RestartFailed
			operation.JobResult = "failed"
		} else {
			operation.State = RestartFailed
			operation.JobResult = "not_started"
		}
	}
	cancel()
	completedAt := time.Now().UTC()
	operation.CompletedAt = &completedAt

	m.mu.Lock()
	record = m.operations[operationID]
	record.Operation = operation
	m.operations[operationID] = record
	delete(m.activeUnits, operation.Unit)
	persistErr := m.persistLocked()
	m.mu.Unlock()
	if persistErr != nil {
		m.logger.Error("persisting service restart result", "operation_id", operation.ID, "unit", operation.Unit)
	}
	m.logger.Info("service restart operation completed", "operation_id", operation.ID, "unit", operation.Unit, "state", operation.State, "job_result", operation.JobResult, "error_code", operation.ErrorCode, "caller", record.Caller)
}

func (m *RestartManager) pruneLocked(now time.Time) {
	for id, record := range m.operations {
		if record.Operation.State == RestartRunning || now.Sub(record.Operation.RequestedAt) <= RestartOperationTTL {
			continue
		}
		delete(m.operations, id)
		delete(m.keys, record.IdempotencyHash)
	}
}

func (m *RestartManager) persistLocked() error {
	records := make([]restartRecord, 0, len(m.operations))
	for _, record := range m.operations {
		records = append(records, record)
	}
	slices.SortFunc(records, func(left, right restartRecord) int {
		if left.Operation.RequestedAt.Before(right.Operation.RequestedAt) {
			return -1
		}
		if left.Operation.RequestedAt.After(right.Operation.RequestedAt) {
			return 1
		}
		return strings.Compare(left.Operation.ID, right.Operation.ID)
	})
	data, err := json.Marshal(restartStore{Version: restartStoreVersion, Records: records})
	if err != nil || len(data) > maxRestartStoreBytes {
		return ErrOperationStore
	}
	directory := filepath.Dir(m.path)
	temporary, err := os.CreateTemp(directory, ".restart-operations-*")
	if err != nil {
		return ErrOperationStore
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return ErrOperationStore
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return ErrOperationStore
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return ErrOperationStore
	}
	if err := temporary.Close(); err != nil {
		return ErrOperationStore
	}
	if err := os.Rename(temporaryName, m.path); err != nil {
		return ErrOperationStore
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return ErrOperationStore
	}
	defer func() { _ = directoryHandle.Close() }()
	if err := directoryHandle.Sync(); err != nil {
		return ErrOperationStore
	}
	return nil
}

func newOperationID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func validOperationID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func validIdempotencyHash(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func validRestartState(state string) bool {
	switch state {
	case RestartRunning, RestartSucceeded, RestartFailed, RestartTimedOut, RestartCancelled, RestartInterrupted:
		return true
	default:
		return false
	}
}

func restartErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrPermissionDenied):
		return "permission_denied"
	case errors.Is(err, ErrNotFound):
		return "unit_not_found"
	case errors.Is(err, ErrUnsupported):
		return "services_unsupported"
	case errors.Is(err, ErrRestartFailed):
		return "restart_failed"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "restart_unavailable"
	}
}

func sanitizeAuditCaller(caller string) string {
	caller = strings.ToValidUTF8(caller, "�")
	if len(caller) > 128 {
		caller = caller[:128]
		caller = strings.ToValidUTF8(caller, "�")
	}
	return strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return '�'
		}
		return character
	}, caller)
}
