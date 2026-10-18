package services

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	maxOutput            = 64 * 1024
	maxProcessStatusSize = 64 * 1024
)

type commandRunner func(context.Context, string) ([]byte, error)

type Systemd struct {
	supported bool
	run       commandRunner
	procRoot  string
}

func NewSystemd() *Systemd {
	_, err := os.Stat("/run/systemd/system")
	return &Systemd{supported: runtime.GOOS == "linux" && err == nil, run: runSystemctl, procRoot: "/proc"}
}

func (s *Systemd) Supported() bool { return s.supported }

func (s *Systemd) Inspect(ctx context.Context, name string) (Unit, error) {
	if ctx.Err() != nil {
		return Unit{}, ctx.Err()
	}
	if !ValidUnit(name) {
		return Unit{}, ErrForbidden
	}
	if !s.supported {
		return Unit{}, ErrUnsupported
	}
	output, err := s.run(ctx, name)
	if ctx.Err() != nil {
		return Unit{}, ctx.Err()
	}
	// systemctl show can exit nonzero for not-found units but still return
	// LoadState=not-found. Never expose raw command output/errors to callers.
	properties := make(map[string]string)
	if len(output) > maxOutput {
		return Unit{}, ErrUnavailable
	}
	for line := range strings.SplitSeq(string(output), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			properties[key] = value
		}
	}
	if properties["LoadState"] == "not-found" {
		return Unit{}, ErrNotFound
	}
	if errors.Is(err, ErrPermissionDenied) {
		return Unit{}, ErrPermissionDenied
	}
	if err != nil {
		return Unit{}, ErrUnavailable
	}
	pid, parseErr := strconv.ParseUint(properties["MainPID"], 10, 32)
	if parseErr != nil || !ValidUnit(properties["Id"]) || properties["LoadState"] == "" || properties["ActiveState"] == "" || properties["SubState"] == "" {
		return Unit{}, ErrUnavailable
	}
	unit := Unit{Name: name, ID: properties["Id"], Description: properties["Description"], LoadState: properties["LoadState"], ActiveState: properties["ActiveState"], SubState: properties["SubState"], UnitFileState: properties["UnitFileState"], MainPID: uint32(pid)}
	if unit.MainPID > 0 {
		unit.MainProcess = s.readProcessIdentity(unit.MainPID)
	}
	return unit, nil
}

func (s *Systemd) readProcessIdentity(pid uint32) *ProcessIdentity {
	if pid == 0 || s.procRoot == "" {
		return nil
	}
	file, err := os.Open(filepath.Join(s.procRoot, strconv.FormatUint(uint64(pid), 10), "status"))
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(io.LimitReader(file, maxProcessStatusSize))
	scanner.Buffer(make([]byte, 1024), maxProcessStatusSize)
	var processName string
	var effectiveUID uint32
	haveUID := false
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		switch key {
		case "Name":
			processName = strings.TrimSpace(value)
		case "Uid":
			ids := strings.Fields(value)
			if len(ids) < 2 {
				return nil
			}
			uid, err := strconv.ParseUint(ids[1], 10, 32)
			if err != nil {
				return nil
			}
			effectiveUID = uint32(uid)
			haveUID = true
		}
		if processName != "" && haveUID {
			processName = strings.ToValidUTF8(processName, "�")
			processName = strings.Map(func(character rune) rune {
				if unicode.IsControl(character) {
					return '�'
				}
				return character
			}, processName)
			return &ProcessIdentity{Name: processName, EffectiveUID: effectiveUID}
		}
	}
	return nil
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if len(data) > maxOutput-b.Len() {
		return 0, errors.New("systemctl output limit exceeded")
	}
	return b.Buffer.Write(data)
}

func runSystemctl(ctx context.Context, name string) ([]byte, error) {
	command := systemctlCommand(ctx, name)
	var output boundedBuffer
	var stderr boundedBuffer
	command.Stdout = &output
	command.Stderr = &stderr
	err := command.Run()
	if err != nil && hasPermissionDenial(stderr.String()) {
		return output.Bytes(), ErrPermissionDenied
	}
	return output.Bytes(), err
}

func hasPermissionDenial(stderr string) bool {
	stderr = strings.ToLower(stderr)
	return strings.Contains(stderr, "permission denied") || strings.Contains(stderr, "access denied") || strings.Contains(stderr, "not authorized")
}

func systemctlCommand(ctx context.Context, name string) *exec.Cmd {
	// No shell, PATH lookup, user-controlled options, inherited environment, or
	// mutation verbs. -- prevents option injection.
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--no-pager", "--property=Id,Description,LoadState,ActiveState,SubState,UnitFileState,MainPID", "--", name)
	command.Env = []string{"LANG=C", "LC_ALL=C", "SYSTEMD_COLORS=0"}
	command.WaitDelay = time.Second
	return command
}
