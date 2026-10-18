package config

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

const (
	DefaultListenAddress     = "127.0.0.1:8787"
	DefaultCollectionTimeout = 10 * time.Second
	DefaultRequestTimeout    = 5 * time.Second
	DefaultDiskPath          = "/"
)

// Config contains the runtime settings for a Stardust agent.
type Config struct {
	ListenAddress     string
	CollectionTimeout time.Duration
	RequestTimeout    time.Duration
	DiskPath          string
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
		return fmt.Errorf("listen address %q is not supported: Stardust has no authentication or TLS and only serves numeric loopback addresses; use SSH port forwarding for remote access", c.ListenAddress)
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
	return nil
}
