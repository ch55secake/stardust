package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateListenAddressPolicy(t *testing.T) {
	tests := []struct {
		name        string
		address     string
		wantValid   bool
		wantMessage string
	}{
		{name: "default IPv4 loopback", address: DefaultListenAddress, wantValid: true},
		{name: "minimum port", address: "127.0.0.1:1", wantValid: true},
		{name: "maximum port", address: "127.0.0.1:65535", wantValid: true},
		{name: "IPv6 loopback", address: "[::1]:8787", wantValid: true},
		{name: "wildcard IPv4", address: "0.0.0.0:8787", wantMessage: "numeric loopback"},
		{name: "wildcard IPv6", address: "[::]:8787", wantMessage: "numeric loopback"},
		{name: "public address", address: "192.0.2.10:8787", wantMessage: "numeric loopback"},
		{name: "localhost hostname", address: "localhost:8787", wantMessage: "numeric loopback"},
		{name: "empty host wildcard", address: ":8787", wantMessage: "numeric loopback"},
		{name: "missing port", address: "127.0.0.1", wantMessage: "numeric loopback address and port"},
		{name: "empty port", address: "127.0.0.1:", wantMessage: "numeric port from 1 to 65535"},
		{name: "non-numeric port", address: "127.0.0.1:not-a-port", wantMessage: "numeric port from 1 to 65535"},
		{name: "port zero is unsupported", address: "127.0.0.1:0", wantMessage: "numeric port from 1 to 65535"},
		{name: "port above TCP range", address: "127.0.0.1:65536", wantMessage: "numeric port from 1 to 65535"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			cfg.ListenAddress = test.address
			err := cfg.Validate()
			if test.wantValid {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			if test.wantMessage != "" && !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err, test.wantMessage)
			}
		})
	}
}

func TestReadAPITokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-token")
	token := strings.Repeat("a", MinAPITokenLength)
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		valid   bool
	}{
		{name: "private with newline", content: token + "\n", mode: 0o600, valid: true},
		{name: "private with CRLF", content: token + "\r\n", mode: 0o600, valid: true},
		{name: "owner read only", content: token, mode: 0o400, valid: true},
		{name: "public mode", content: token, mode: 0o644},
		{name: "group readable", content: token, mode: 0o640},
		{name: "too short", content: "short", mode: 0o600},
		{name: "leading whitespace", content: " " + token, mode: 0o600},
		{name: "trailing whitespace", content: token + " ", mode: 0o600},
		{name: "embedded whitespace", content: token[:16] + " " + token[16:], mode: 0o600},
		{name: "non ASCII", content: token[:31] + "é", mode: 0o600},
		{name: "too large", content: strings.Repeat("a", MaxAPITokenFileSize+1), mode: 0o600},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, test.mode); err != nil {
				t.Fatal(err)
			}
			got, err := ReadAPITokenFile(path)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, token=%q, error=%v", test.valid, got, err)
			}
			if test.valid && got != token {
				t.Fatalf("token = %q, want configured token", got)
			}
			if err != nil && strings.Contains(err.Error(), token) {
				t.Fatal("token leaked in error")
			}
		})
	}
	if got, err := ReadAPITokenFile(""); err != nil || got != "" {
		t.Fatalf("empty path should disable authentication: token=%q error=%v", got, err)
	}
	link := filepath.Join(t.TempDir(), "token-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAPITokenFile(link); err == nil {
		t.Fatal("symbolic link accepted")
	}
	if _, err := ReadAPITokenFile(t.TempDir()); err == nil {
		t.Fatal("directory accepted")
	}
	unsafeDirectory := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(unsafeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeDirectory, 0o777); err != nil {
		t.Fatal(err)
	}
	unsafePath := filepath.Join(unsafeDirectory, "token")
	if err := os.WriteFile(unsafePath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAPITokenFile(unsafePath); err == nil {
		t.Fatal("token in group/other-writable directory accepted")
	}
}

func TestValidateAPITokenFilePath(t *testing.T) {
	cfg := Default()
	cfg.APITokenFile = " \t "
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "API token file path") {
		t.Fatalf("Validate() error = %v, want invalid token file path", err)
	}
}
