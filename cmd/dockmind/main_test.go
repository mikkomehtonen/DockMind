package main

import (
	"testing"

	"github.com/dockmind/dockmind/internal/config"
)

func TestNewLactClient(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *config.Config
		wantNil bool
	}{
		{"enabled returns client", &config.Config{Lact: config.LactConfig{Enabled: true}}, false},
		{"disabled returns nil", &config.Config{Lact: config.LactConfig{Enabled: false}}, true},
		{"absent section returns nil", &config.Config{}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newLactClient(tc.cfg)
			if tc.wantNil {
				if got != nil {
					t.Fatal("expected nil lact client")
				}
			} else {
				if got == nil {
					t.Fatal("expected non-nil lact client")
				}
			}
		})
	}
}
