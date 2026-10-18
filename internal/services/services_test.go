package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef"

func TestPolicyValidationAndAuthorization(t *testing.T) {
	policy, err := NewPolicy(testToken, []string{"z.service", "a.service", "a.service"})
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Authorized(testToken) || policy.Authorized("") || policy.Authorized(testToken+"x") {
		t.Fatal("incorrect token authorization")
	}
	if !policy.Allows("a.service") || policy.Allows("other.service") {
		t.Fatal("incorrect unit authorization")
	}
	if !reflect.DeepEqual(policy.Units(), []string{"a.service", "z.service"}) {
		t.Fatal("allowlist is not sorted/deduplicated")
	}
	units := policy.Units()
	units[0] = "changed.service"
	if !policy.Allows("a.service") {
		t.Fatal("caller mutated policy")
	}
	for _, token := range []string{"", "short", strings.Repeat("a", 257), testToken + "\n", testToken + "é"} {
		if _, err := NewPolicy(token, nil); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
	for _, name := range []string{"", ".service", "../../a.service", "*.service", "--help.service", "a\n.service", "x.socket", "a b.service", "a;id.service", "x?.service", strings.Repeat("x", 256) + ".service"} {
		if ValidUnit(name) {
			t.Errorf("invalid name accepted: %q", name)
		}
		if _, err := NewPolicy(testToken, []string{name}); err == nil {
			t.Errorf("invalid policy name accepted: %q", name)
		}
	}
	for _, name := range []string{"sshd.service", "worker@blue.service", `worker@a\x2db.service`, "db-backup.service"} {
		if !ValidUnit(name) {
			t.Errorf("valid name rejected: %q", name)
		}
	}
	if _, err := NewPolicy(testToken, make([]string, MaxUnits+1)); err == nil {
		t.Fatal("oversized allowlist accepted")
	}
	var disabled *Policy
	if disabled.Authorized(testToken) || disabled.Allows("a.service") {
		t.Fatal("nil policy allowed access")
	}
}

func TestLogReadPolicyHasIndependentCredentialAndAllowlist(t *testing.T) {
	logToken := "abcdef0123456789abcdef0123456789"
	policy, err := NewPolicyWithLogs(testToken, []string{"web.service"}, logToken, []string{"web.service", "worker@blue.service"})
	if err != nil {
		t.Fatal(err)
	}
	logs := policy.LogReadPolicy()
	if logs == nil || !logs.Authorized(logToken) || logs.Authorized(testToken) || policy.Authorized(logToken) {
		t.Fatal("inventory and log credentials were not isolated")
	}
	if logs.Allows("web.service") != true || logs.Allows("worker@blue.service") != true || logs.Allows("private.service") {
		t.Fatal("log allowlist incorrect")
	}
	if policy.Allows("private.service") {
		t.Fatal("log allowlist widened service inventory")
	}
	if _, err := NewPolicyWithLogs(testToken, nil, testToken, nil); err == nil {
		t.Fatal("shared service/log token accepted")
	}
	if _, err := NewPolicyWithLogs(testToken, nil, logToken, []string{"*.service"}); err == nil {
		t.Fatal("invalid log unit accepted")
	}
}

func TestLoadPolicyPrivateFileAndSafeErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	valid := `{"token":"` + testToken + `","units":["sshd.service"]}`
	for _, test := range []struct {
		name, body string
		mode       os.FileMode
		valid      bool
	}{
		{"private", valid, 0600, true},
		{"readonly", valid, 0400, true},
		{"empty allowlist", `{"token":"` + testToken + `","units":[]}`, 0600, true},
		{"separate log policy", `{"token":"` + testToken + `","units":["web.service"],"logs":{"token":"abcdef0123456789abcdef0123456789","units":["web.service"]}}`, 0600, true},
		{"same log token", `{"token":"` + testToken + `","units":[],"logs":{"token":"` + testToken + `","units":[]}}`, 0600, false},
		{"invalid log unit", `{"token":"` + testToken + `","units":[],"logs":{"token":"abcdef0123456789abcdef0123456789","units":["*.service"]}}`, 0600, false},
		{"public", valid, 0644, false},
		{"group readable", valid, 0640, false},
		{"invalid JSON", testToken, 0600, false},
		{"unknown key", `{"token":"` + testToken + `","restart":true}`, 0600, false},
		{"trailing JSON", valid + ` {}`, 0600, false},
		{"too large", strings.Repeat(" ", 65537), 0600, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.Chmod(path, 0600); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, test.mode); err != nil {
				t.Fatal(err)
			}
			_, err := LoadPolicy(path)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), testToken) {
				t.Fatal("secret leaked in policy error")
			}
		})
	}
	if _, err := LoadPolicy(t.TempDir()); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := LoadPolicy(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(link); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestSystemdInspection(t *testing.T) {
	output := "Id=actual.service\nDescription=Example service\nLoadState=loaded\nActiveState=active\nSubState=running\nUnitFileState=enabled\nMainPID=123\n"
	procRoot := t.TempDir()
	writeProcessStatus(t, procRoot, "123", "Name:\tworker\nUid:\t1000\t1001\t1000\t1001\n")
	inspector := &Systemd{supported: true, run: func(_ context.Context, name string) ([]byte, error) {
		if name != "alias.service" {
			t.Fatalf("unexpected unit %s", name)
		}
		return []byte(output), nil
	}, procRoot: procRoot}
	unit, err := inspector.Inspect(context.Background(), "alias.service")
	if err != nil {
		t.Fatal(err)
	}
	if unit.Name != "alias.service" || unit.ID != "actual.service" || unit.MainPID != 123 || unit.UnitFileState != "enabled" || !reflect.DeepEqual(unit.MainProcess, &ProcessIdentity{Name: "worker", EffectiveUID: 1001}) {
		t.Fatalf("unexpected unit: %+v", unit)
	}
	output = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(output, "active\n", "inactive\n"), "running", "dead"), "123", "0")
	unit, err = inspector.Inspect(context.Background(), "alias.service")
	if err != nil || unit.MainPID != 0 || unit.ActiveState != "inactive" || unit.MainProcess != nil {
		t.Fatalf("inactive/no PID: %+v %v", unit, err)
	}
	output = strings.ReplaceAll(strings.ReplaceAll(output, "inactive", "failed"), "dead", "failed")
	unit, err = inspector.Inspect(context.Background(), "alias.service")
	if err != nil || unit.ActiveState != "failed" || unit.SubState != "failed" {
		t.Fatalf("failed unit lost: %+v %v", unit, err)
	}
	for _, test := range []struct {
		output    string
		err, want error
	}{
		{"LoadState=not-found\n", errors.New("exit status 1"), ErrNotFound},
		{"", errors.New("permission denied: secret details"), ErrUnavailable},
		{"MainPID=nope\n", nil, ErrUnavailable},
		{strings.Repeat("x", maxOutput+1), nil, ErrUnavailable},
	} {
		inspector.run = func(context.Context, string) ([]byte, error) { return []byte(test.output), test.err }
		if _, err := inspector.Inspect(context.Background(), "alias.service"); !errors.Is(err, test.want) {
			t.Fatalf("got %v want %v", err, test.want)
		}
	}
	inspector.run = func(context.Context, string) ([]byte, error) {
		t.Fatal("invalid/unsupported query reached backend")
		return nil, nil
	}
	if _, err := inspector.Inspect(context.Background(), "bad;id.service"); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	inspector.supported = false
	if _, err := inspector.Inspect(context.Background(), "alias.service"); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
}

func TestSystemdProcessIdentityTracksChangingPIDsAndIsBestEffort(t *testing.T) {
	procRoot := t.TempDir()
	writeProcessStatus(t, procRoot, "123", "Name:\tworker-old\nUid:\t1000\t1001\t1000\t1001\n")
	writeProcessStatus(t, procRoot, "456", "Name:\tworker-new\nUid:\t1000\t1002\t1000\t1002\n")
	pid := "123"
	inspector := &Systemd{
		supported: true,
		procRoot:  procRoot,
		run: func(context.Context, string) ([]byte, error) {
			return []byte("Id=worker.service\nLoadState=loaded\nActiveState=active\nSubState=running\nMainPID=" + pid + "\n"), nil
		},
	}
	first, err := inspector.Inspect(context.Background(), "worker.service")
	if err != nil || first.MainPID != 123 || !reflect.DeepEqual(first.MainProcess, &ProcessIdentity{Name: "worker-old", EffectiveUID: 1001}) {
		t.Fatalf("first inspection = %+v, %v", first, err)
	}
	pid = "456"
	second, err := inspector.Inspect(context.Background(), "worker.service")
	if err != nil || second.MainPID != 456 || !reflect.DeepEqual(second.MainProcess, &ProcessIdentity{Name: "worker-new", EffectiveUID: 1002}) {
		t.Fatalf("second inspection = %+v, %v", second, err)
	}
	pid = "789"
	missing, err := inspector.Inspect(context.Background(), "worker.service")
	if err != nil || missing.MainPID != 789 || missing.MainProcess != nil {
		t.Fatalf("missing proc entry should keep unit state without process identity: %+v, %v", missing, err)
	}
}

func TestProcessIdentitySanitizesKernelName(t *testing.T) {
	procRoot := t.TempDir()
	writeProcessStatus(t, procRoot, "123", "Name:\tworker\x1b[31m\nUid:\t1000\t1001\t1000\t1001\n")
	inspector := &Systemd{procRoot: procRoot}
	identity := inspector.readProcessIdentity(123)
	if identity == nil || strings.ContainsRune(identity.Name, '\x1b') || identity.Name != "worker�[31m" {
		t.Fatalf("unsafe process identity: %+v", identity)
	}
}

func writeProcessStatus(t *testing.T, procRoot, pid, content string) {
	t.Helper()
	directory := filepath.Join(procRoot, pid)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "status"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledInspectionAndBoundedOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inspector := &Systemd{supported: true, run: func(ctx context.Context, _ string) ([]byte, error) { return nil, ctx.Err() }}
	if _, err := inspector.Inspect(ctx, "sshd.service"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var buffer boundedBuffer
	if _, err := buffer.Write(make([]byte, maxOutput)); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte("x")); err == nil || buffer.Len() != maxOutput {
		t.Fatal("output exceeded limit")
	}
}

func TestPermissionDenialClassificationDoesNotExposeStderr(t *testing.T) {
	for _, stderr := range []string{
		"Failed to get properties: Access denied.",
		"permission denied while reading service properties",
		"Not authorized to perform operation",
	} {
		if !hasPermissionDenial(stderr) {
			t.Errorf("permission denial not recognized: %q", stderr)
		}
	}
	if hasPermissionDenial("Failed to connect to bus: No such file or directory") {
		t.Fatal("unavailable bus misclassified as permission denial")
	}
	inspector := &Systemd{supported: true, run: func(context.Context, string) ([]byte, error) {
		return nil, ErrPermissionDenied
	}}
	if _, err := inspector.Inspect(context.Background(), "worker.service"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("inspection error = %v, want permission denied", err)
	}
}

func TestSystemctlCommandIsReadOnlyAndDoesNotUseShell(t *testing.T) {
	command := systemctlCommand(context.Background(), "worker@blue.service")
	want := []string{"/usr/bin/systemctl", "show", "--no-pager", "--property=Id,Description,LoadState,ActiveState,SubState,UnitFileState,MainPID", "--", "worker@blue.service"}
	if command.Path != "/usr/bin/systemctl" || !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("unsafe command: %+v", command.Args)
	}
	if !reflect.DeepEqual(command.Env, []string{"LANG=C", "LC_ALL=C", "SYSTEMD_COLORS=0"}) || command.WaitDelay <= 0 {
		t.Fatal("unbounded cleanup or inherited environment")
	}
}
