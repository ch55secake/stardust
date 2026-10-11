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
		{name: "IPv6 loopback", address: "[::1]:8787", wantValid: true},
		{name: "wildcard IPv4", address: "0.0.0.0:8787", wantMessage: "no authentication or TLS"},
		{name: "wildcard IPv6", address: "[::]:8787", wantMessage: "no authentication or TLS"},
		{name: "public address", address: "192.0.2.10:8787", wantMessage: "no authentication or TLS"},
		{name: "localhost hostname", address: "localhost:8787", wantMessage: "numeric loopback"},
		{name: "empty host wildcard", address: ":8787", wantMessage: "no authentication or TLS"},
		{name: "missing port", address: "127.0.0.1", wantMessage: "numeric loopback address and port"},
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
