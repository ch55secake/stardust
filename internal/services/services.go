// Package services provides read-only, explicitly allowlisted systemd metadata.
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
	ErrUnsupported      = errors.New("systemd service inventory is unsupported on this host")
	ErrUnavailable      = errors.New("systemd is unavailable")
	ErrPermissionDenied = errors.New("systemd access was denied")
	ErrNotFound         = errors.New("service unit not found")
	ErrForbidden        = errors.New("service unit is not allowed")
	unitPattern         = regexp.MustCompile(`^[A-Za-z0-9_:@.\\-]+\.service$`)
)

const MaxUnits = 128

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

// Policy is immutable after construction. The bearer token is never exported.
type Policy struct {
	tokenHash [32]byte
	units     []string
	logRead   *Policy
}

func ValidUnit(name string) bool {
	return len(name) <= 255 && unitPattern.MatchString(name) && name != ".service" && !strings.Contains(name, "..") && !strings.HasPrefix(name, "-")
}

func NewPolicy(token string, units []string) (*Policy, error) {
	return newPolicy(token, units)
}

// NewPolicyWithLogs enables an independently authenticated, independently
// allowlisted journal-read capability. The log credential cannot be the service
// inventory credential, preventing an inventory client from inheriting log access.
func NewPolicyWithLogs(token string, units []string, logToken string, logUnits []string) (*Policy, error) {
	policy, err := newPolicy(token, units)
	if err != nil {
		return nil, err
	}
	if token == logToken {
		return nil, errors.New("service and log read tokens must be different")
	}
	policy.logRead, err = newPolicy(logToken, logUnits)
	if err != nil {
		return nil, fmt.Errorf("invalid log read policy: %w", err)
	}
	return policy, nil
}

func newPolicy(token string, units []string) (*Policy, error) {
	if len(token) < 32 || len(token) > 256 || strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
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
	if config.Logs == nil {
		return NewPolicy(config.Token, config.Units)
	}
	return NewPolicyWithLogs(config.Token, config.Units, config.Logs.Token, config.Logs.Units)
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
