package config

import (
	"fmt"
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
