package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const maxOutput = 64 * 1024

type commandRunner func(context.Context, string) ([]byte, error)

type Systemd struct {
	supported bool
	run       commandRunner
}

func NewSystemd() *Systemd {
	_, err := os.Stat("/run/systemd/system")
	return &Systemd{supported: runtime.GOOS == "linux" && err == nil, run: runSystemctl}
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
	if err != nil {
		return Unit{}, ErrUnavailable
	}
	pid, parseErr := strconv.ParseUint(properties["MainPID"], 10, 32)
	if parseErr != nil || !ValidUnit(properties["Id"]) || properties["LoadState"] == "" || properties["ActiveState"] == "" || properties["SubState"] == "" {
		return Unit{}, ErrUnavailable
	}
	return Unit{Name: name, ID: properties["Id"], Description: properties["Description"], LoadState: properties["LoadState"], ActiveState: properties["ActiveState"], SubState: properties["SubState"], UnitFileState: properties["UnitFileState"], MainPID: uint32(pid)}, nil
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
	command.Stdout = &output
	command.Stderr = io.Discard
	err := command.Run()
	return output.Bytes(), err
}

func systemctlCommand(ctx context.Context, name string) *exec.Cmd {
	// No shell, PATH lookup, user-controlled options, environment overrides,
	// process arguments, or mutation verbs. -- prevents option injection.
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--no-pager", "--property=Id,Description,LoadState,ActiveState,SubState,UnitFileState,MainPID", "--", name)
	command.Env = []string{"LANG=C", "LC_ALL=C", "SYSTEMD_COLORS=0"}
	command.WaitDelay = time.Second
	return command
}
