package config

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ch55secake/stardust/internal/services"
)

const (
	DefaultListenAddress     = "127.0.0.1:8787"
	DefaultCollectionTimeout = 10 * time.Second
	DefaultRequestTimeout    = 5 * time.Second
	DefaultDiskPath          = "/"
	MinAPITokenLength        = 32
	MaxAPITokenFileSize      = 4096
)

// Config contains the runtime settings for a Stardust agent.
type Config struct {
	ListenAddress     string
	CollectionTimeout time.Duration
	RequestTimeout    time.Duration
	DiskPath          string
	ServicePolicy     *services.Policy
	APITokenFile      string
}

func Default() Config {
	return Config{
		ListenAddress:     DefaultListenAddress,
		CollectionTimeout: DefaultCollectionTimeout,
		RequestTimeout:    DefaultRequestTimeout,
		DiskPath:          DefaultDiskPath,
	}
}

func (c Config) Validate() error {
	if c.ListenAddress == "" {
		return fmt.Errorf("listen address must not be empty")
	}
	host, portString, err := net.SplitHostPort(c.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen address %q must be a numeric loopback address and port: %w", c.ListenAddress, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address %q is not supported: Stardust only serves numeric loopback addresses and does not support TLS; use SSH port forwarding for remote access", c.ListenAddress)
	}
	port, err := strconv.ParseUint(portString, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("listen address %q must use a numeric port from 1 to 65535", c.ListenAddress)
	}
	if c.CollectionTimeout <= 0 {
		return fmt.Errorf("collection timeout must be positive")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request timeout must be positive")
	}
	if c.DiskPath == "" {
		return fmt.Errorf("disk path must not be empty")
	}
	if c.APITokenFile != "" && strings.TrimSpace(c.APITokenFile) == "" {
		return fmt.Errorf("API token file path must not be empty")
	}
	return nil
}

// ReadAPITokenFile loads an optional bearer token from a private regular file
// owned by the runtime user. The token itself is never included in errors.
func ReadAPITokenFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect API token file %q: %w", path, err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("API token file %q must not be a symbolic link", path)
	}
	if err := validateAPITokenDirectory(path); err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open API token file %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect API token file %q: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !os.SameFile(linkInfo, info) || !info.Mode().IsRegular() || (info.Mode().Perm() != 0o400 && info.Mode().Perm() != 0o600) || !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("API token file %q must be an unchanged regular file owned by the runtime user with mode 0400 or 0600", path)
	}
	content, err := io.ReadAll(io.LimitReader(file, MaxAPITokenFileSize+1))
	if err != nil {
		return "", fmt.Errorf("read API token file %q: %w", path, err)
	}
	if len(content) > MaxAPITokenFileSize {
		return "", fmt.Errorf("API token file %q exceeds %d bytes", path, MaxAPITokenFileSize)
	}
	token := strings.TrimSuffix(string(content), "\n")
	token = strings.TrimSuffix(token, "\r")
	if len(token) < MinAPITokenLength {
		return "", fmt.Errorf("API token file %q must contain at least %d characters", path, MinAPITokenLength)
	}
	for _, character := range []byte(token) {
		if character < 0x21 || character > 0x7e {
			return "", fmt.Errorf("API token file %q must contain printable non-whitespace ASCII only", path)
		}
	}
	return token, nil
}

func validateAPITokenDirectory(path string) error {
	directory, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("resolve API token file directory: %w", err)
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("resolve API token file directory: %w", err)
	}
	for {
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("API token file %q must be in root- or runtime-owned directories not writable by group or other users", path)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
			return fmt.Errorf("API token file %q must be in root- or runtime-owned directories not writable by group or other users", path)
		}
		writableByOthers := info.Mode().Perm()&0o022 != 0
		rootStickyDirectory := stat.Uid == 0 && info.Mode()&os.ModeSticky != 0
		if writableByOthers && !rootStickyDirectory {
			return fmt.Errorf("API token file %q must be in root- or runtime-owned directories not writable by group or other users", path)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return nil
		}
		directory = parent
	}
}
