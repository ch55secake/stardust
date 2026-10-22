// Package services provides explicitly allowlisted systemd metadata and control.
package services

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

var (
	ErrUnsupported         = errors.New("systemd service inventory is unsupported on this host")
	ErrUnavailable         = errors.New("systemd is unavailable")
	ErrPermissionDenied    = errors.New("systemd access was denied")
	ErrNotFound            = errors.New("service unit not found")
	ErrForbidden           = errors.New("service unit is not allowed")
	ErrRestartFailed       = errors.New("systemd restart job failed")
	ErrRestartDisabled     = errors.New("service restart is disabled")
	ErrUnitBusy            = errors.New("service already has a restart operation in progress")
	ErrIdempotencyConflict = errors.New("idempotency key was already used for a different unit")
	ErrOperationNotFound   = errors.New("restart operation not found")
	ErrOperationStore      = errors.New("restart operation store is unavailable")
	ErrOperationLimit      = errors.New("restart operation capacity is full")
	unitPattern            = regexp.MustCompile(`^[A-Za-z0-9_:@.\\-]+\.service$`)
	restartUnitPattern     = regexp.MustCompile(`^[A-Za-z0-9_:@.-]+\.service$`)
)

const (
	MaxUnits        = 128
	MaxRestartUnits = 32
)

type Unit struct {
	Name          string           `json:"name"`
	ID            string           `json:"id"`
	Description   string           `json:"description"`
	LoadState     string           `json:"load_state"`
	ActiveState   string           `json:"active_state"`
	SubState      string           `json:"sub_state"`
	UnitFileState string           `json:"unit_file_state"`
	MainPID       uint32           `json:"main_pid"`
	MainProcess   *ProcessIdentity `json:"main_process,omitempty"`
	// RestartAllowed is an informational API preflight; the restart endpoint remains authoritative.
	RestartAllowed bool             `json:"restart_allowed,omitempty"`
	Diagnostics    *UnitDiagnostics `json:"diagnostics,omitempty"`
}

// UnitDiagnostics contains optional systemd invocation diagnostics. These are
// operational hints, not an application health check.
type UnitDiagnostics struct {
	Result         string  `json:"result,omitempty"`
	ExecMainCode   *int32  `json:"exec_main_code,omitempty"`
	ExecMainStatus *int32  `json:"exec_main_status,omitempty"`
	RestartCount   *uint64 `json:"restart_count,omitempty"`
}

type ProcessIdentity struct {
	Name         string `json:"name"`
	EffectiveUID uint32 `json:"effective_uid"`
}

// Inspector must not execute mutations or expose command lines/environment.
type Inspector interface {
	Supported() bool
	Inspect(context.Context, string) (Unit, error)
}

// Restarter performs exactly one allowlisted systemd restart; it must not accept
// arbitrary commands or expose process configuration.
type Restarter interface {
	Supported() bool
	Restart(context.Context, string) error
}

// Policy is immutable after construction. The bearer token is never exported.
type Policy struct {
	tokenHash        [32]byte
	units            []string
	logRead          *Policy
	restartTokenHash [32]byte
	restartUnits     []string
}

func ValidUnit(name string) bool {
	return len(name) <= 255 && unitPattern.MatchString(name) && name != ".service" && !strings.Contains(name, "..") && !strings.HasPrefix(name, "-")
}

// ValidRestartUnit excludes backslash escapes so an exact unit argument can be
// represented safely in a narrowly scoped PolicyKit rule without pattern syntax.
func ValidRestartUnit(name string) bool {
	return ValidUnit(name) && restartUnitPattern.MatchString(name)
}

func NewPolicy(token string, units []string) (*Policy, error) {
	return NewPolicyWithLogsAndRestart(token, units, "", nil, "", nil)
}

// NewPolicyWithLogs enables an independently authenticated, independently
// allowlisted journal-read capability.
func NewPolicyWithLogs(token string, units []string, logToken string, logUnits []string) (*Policy, error) {
	if _, err := newPolicy(logToken, logUnits); err != nil {
		return nil, fmt.Errorf("invalid log read policy: %w", err)
	}
	return NewPolicyWithLogsAndRestart(token, units, logToken, logUnits, "", nil)
}

// NewPolicyWithRestart independently configures read and restart authority.
func NewPolicyWithRestart(token string, units []string, restartToken string, restartUnits []string) (*Policy, error) {
	return NewPolicyWithLogsAndRestart(token, units, "", nil, restartToken, restartUnits)
}

// NewPolicyWithLogsAndRestart creates independent read, journal-read, and
// restart credentials. Restart units must also be visible in inventory.
func NewPolicyWithLogsAndRestart(token string, units []string, logToken string, logUnits []string, restartToken string, restartUnits []string) (*Policy, error) {
	policy, err := newPolicy(token, units)
	if err != nil {
		return nil, err
	}
	readTokenHash := policy.tokenHash
	if logToken != "" || logUnits != nil {
		logTokenHash := sha256.Sum256([]byte(logToken))
		if subtle.ConstantTimeCompare(readTokenHash[:], logTokenHash[:]) == 1 {
			return nil, errors.New("service and log read tokens must be different")
		}
		policy.logRead, err = newPolicy(logToken, logUnits)
		if err != nil {
			return nil, fmt.Errorf("invalid log read policy: %w", err)
		}
	}
	if restartToken == "" && len(restartUnits) == 0 {
		return policy, nil
	}
	if !validServiceToken(restartToken) {
		return nil, errors.New("restart token must contain 32–256 printable non-space ASCII characters")
	}
	if len(restartUnits) == 0 {
		return nil, errors.New("restart token requires a non-empty restart unit allowlist")
	}
	if len(restartUnits) > MaxRestartUnits {
		return nil, fmt.Errorf("restart allowlist exceeds %d units", MaxRestartUnits)
	}
	restartTokenHash := sha256.Sum256([]byte(restartToken))
	if subtle.ConstantTimeCompare(readTokenHash[:], restartTokenHash[:]) == 1 {
		return nil, errors.New("service read and restart tokens must be different")
	}
	if policy.logRead != nil && subtle.ConstantTimeCompare(policy.logRead.tokenHash[:], restartTokenHash[:]) == 1 {
		return nil, errors.New("log read and restart tokens must be different")
	}
	restartUnits = slices.Clone(restartUnits)
	for _, unit := range restartUnits {
		if !ValidRestartUnit(unit) || !slices.Contains(policy.units, unit) {
			return nil, errors.New("restart allowlist must contain valid units from the service inventory allowlist")
		}
	}
	slices.Sort(restartUnits)
	policy.restartTokenHash = restartTokenHash
	policy.restartUnits = slices.Compact(restartUnits)
	return policy, nil
}

func newPolicy(token string, units []string) (*Policy, error) {
	if !validServiceToken(token) {
		return nil, errors.New("service token must contain 32–256 printable non-space ASCII characters")
	}
	if len(units) > MaxUnits {
		return nil, fmt.Errorf("service allowlist exceeds %d units", MaxUnits)
	}
	units = slices.Clone(units)
	for _, unit := range units {
		if !ValidUnit(unit) {
			return nil, errors.New("service allowlist contains an invalid .service unit name")
		}
	}
	slices.Sort(units)
	return &Policy{tokenHash: sha256.Sum256([]byte(token)), units: slices.Compact(units)}, nil
}

func validServiceToken(token string) bool {
	return len(token) >= 32 && len(token) <= 256 && strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) < 0
}

// LoadPolicy reads credentials from a regular, private file owned by the daemon
// user or root. Errors never contain its contents. The path, not the secret, may
// be passed on the command line. An empty allowlist deliberately grants nothing.
func LoadPolicy(path string) (*Policy, error) {
	linkInfo, err := os.Lstat(path)
	if err != nil || !linkInfo.Mode().IsRegular() {
		return nil, errors.New("service policy must be a regular file, not a symbolic link or device")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open service policy file")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New("cannot stat service policy file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !os.SameFile(linkInfo, info) || !info.Mode().IsRegular() || (info.Mode().Perm() != 0400 && info.Mode().Perm() != 0600) || !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return nil, errors.New("service policy must be an unchanged regular file owned by root or the daemon user, with mode 0400 or 0600")
	}
	if info.Size() > 64*1024 {
		return nil, errors.New("service policy file is too large")
	}
	var config struct {
		Token string   `json:"token"`
		Units []string `json:"units"`
		Logs  *struct {
			Token string   `json:"token"`
			Units []string `json:"units"`
		} `json:"logs,omitempty"`
		RestartToken string   `json:"restart_token"`
		RestartUnits []string `json:"restart_units"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, errors.New("invalid service policy JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("invalid trailing service policy data")
	}
	if config.Logs != nil {
		if _, err := newPolicy(config.Logs.Token, config.Logs.Units); err != nil {
			return nil, errors.New("invalid log read policy")
		}
	}
	logToken, logUnits := "", []string(nil)
	if config.Logs != nil {
		logToken, logUnits = config.Logs.Token, config.Logs.Units
	}
	return NewPolicyWithLogsAndRestart(config.Token, config.Units, logToken, logUnits, config.RestartToken, config.RestartUnits)
}

func (p *Policy) Authorized(token string) bool {
	if p == nil {
		return false
	}
	hash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(hash[:], p.tokenHash[:]) == 1
}

func (p *Policy) Units() []string { return slices.Clone(p.units) }

// LogReadPolicy returns the separately scoped immutable log-read policy.
func (p *Policy) LogReadPolicy() *Policy {
	if p == nil {
		return nil
	}
	return p.logRead
}

func (p *Policy) Allows(unit string) bool {
	if p == nil {
		return false
	}
	_, found := slices.BinarySearch(p.units, unit)
	return found
}

func (p *Policy) RestartAuthorized(token string) bool {
	if p == nil || len(p.restartUnits) == 0 {
		return false
	}
	hash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(hash[:], p.restartTokenHash[:]) == 1
}

func (p *Policy) RestartEnabled() bool { return p != nil && len(p.restartUnits) > 0 }

func (p *Policy) RestartUnits() []string { return slices.Clone(p.restartUnits) }

func (p *Policy) AllowsRestart(unit string) bool {
	if p == nil {
		return false
	}
	_, found := slices.BinarySearch(p.restartUnits, unit)
	return found
}
