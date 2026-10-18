package config

import (
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
