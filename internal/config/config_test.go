package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tmp := t.TempDir()

	cases := []struct {
		name    string
		content string
		want    Config
		wantErr bool
	}{
		{
			name: "full config",
			content: `server:
  address: ":8080"
shelly:
  address: 192.168.1.50
  channel: 0
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
gpu:
  pollInterval: 1s
startup:
  timeout: 60s
shutdown:
  timeout: 30s
`,
			want: Config{
				Server:    ServerConfig{Address: ":8080"},
				Shelly:    ShellyConfig{Address: "192.168.1.50", Channel: 0},
				Docker:    DockerConfig{Container: "llama-swap"},
				LlamaSwap: LlamaSwapConfig{HealthURL: "http://localhost:1234/v1/models"},
				GPU:       GPUConfig{PollInterval: Duration(time.Second)},
				Startup:   StartupConfig{Timeout: Duration(60 * time.Second)},
				Shutdown:  ShutdownConfig{Timeout: Duration(30 * time.Second)},
			},
		},
		{
			name: "minimal config",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
`,
			want: Config{
				Server:    ServerConfig{Address: ":8080"},
				Shelly:    ShellyConfig{Address: "192.168.1.50", Channel: 0},
				Docker:    DockerConfig{Container: "llama-swap"},
				LlamaSwap: LlamaSwapConfig{HealthURL: "http://localhost:1234/v1/models"},
				GPU:       GPUConfig{PollInterval: Duration(time.Second)},
				Startup:   StartupConfig{Timeout: Duration(60 * time.Second)},
				Shutdown:  ShutdownConfig{Timeout: Duration(30 * time.Second)},
			},
		},
		{
			name:    "missing file",
			content: "",
			wantErr: true,
		},
		{
			name: "malformed YAML",
			content: `shelly: address: 192.168.1.50
`,
			wantErr: true,
		},
		{
			name: "invalid duration",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
gpu:
  pollInterval: abc
`,
			wantErr: true,
		},
		{
			name: "missing required field",
			content: `docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
`,
			wantErr: true,
		},
		{
			name: "negative poll interval",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
gpu:
  pollInterval: -1s
`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if tc.content != "" {
				path = filepath.Join(tmp, tc.name+".yaml")
				if err := os.WriteFile(path, []byte(tc.content), 0644); err != nil {
					t.Fatalf("write file: %v", err)
				}
			} else {
				path = filepath.Join(tmp, "does-not-exist.yaml")
			}

			cfg, err := Load(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if *cfg != tc.want {
				t.Fatalf("config mismatch:\n got: %+v\nwant: %+v", *cfg, tc.want)
			}
		})
	}
}

func TestLoadCustomPath(t *testing.T) {
	tmp := t.TempDir()
	custom := filepath.Join(tmp, "custom.yaml")
	content := []byte(`shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
`)
	if err := os.WriteFile(custom, content, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cfg, err := Load(custom)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Docker.Container != "llama-swap" {
		t.Fatalf("expected container llama-swap, got %q", cfg.Docker.Container)
	}
}

func TestGatewayConfig(t *testing.T) {
	tmp := t.TempDir()

	cases := []struct {
		name    string
		content string
		assert  func(t *testing.T, cfg *Config)
		wantErr bool
		errSub  string // substring to check in error message
	}{
		{
			name: "gateway enabled with backendUrl",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: http://localhost:1234
gateway:
  enabled: true
`,
			assert: func(t *testing.T, cfg *Config) {
				if !cfg.Gateway.Enabled {
					t.Error("expected gateway enabled")
				}
				if cfg.LlamaSwap.BackendURL != "http://localhost:1234" {
					t.Errorf("expected backendUrl http://localhost:1234, got %q", cfg.LlamaSwap.BackendURL)
				}
				// requestTimeout should default to 120s when enabled and zero
				if cfg.Gateway.RequestTimeout != Duration(120*time.Second) {
					t.Errorf("expected requestTimeout 120s, got %v", cfg.Gateway.RequestTimeout.Duration())
				}
			},
		},
		{
			name: "gateway enabled with empty backendUrl fails",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
gateway:
  enabled: true
`,
			wantErr: true,
			errSub:  "backendUrl",
		},
		{
			name: "gateway enabled with invalid backendUrl fails",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: "not a url"
gateway:
  enabled: true
`,
			wantErr: true,
		},
		{
			name: "gateway disabled with empty backendUrl ok",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
gateway:
  enabled: false
`,
			assert: func(t *testing.T, cfg *Config) {
				if cfg.Gateway.Enabled {
					t.Error("expected gateway disabled")
				}
			},
		},
		{
			name: "no gateway section with empty backendUrl ok",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
`,
			assert: func(t *testing.T, cfg *Config) {
				if cfg.Gateway.Enabled {
					t.Error("expected gateway disabled by default")
				}
			},
		},
		{
			name: "gateway enabled with idleTimeout 0s ok",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: http://localhost:1234
gateway:
  enabled: true
  idleTimeout: 0s
`,
			assert: func(t *testing.T, cfg *Config) {
				if cfg.Gateway.IdleTimeout != Duration(0) {
					t.Errorf("expected idleTimeout 0, got %v", cfg.Gateway.IdleTimeout.Duration())
				}
			},
		},
		{
			name: "gateway enabled with negative idleTimeout fails",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: http://localhost:1234
gateway:
  enabled: true
  idleTimeout: -1s
`,
			wantErr: true,
		},
		{
			name: "gateway enabled with requestTimeout absent defaults to 120s",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: http://localhost:1234
gateway:
  enabled: true
`,
			assert: func(t *testing.T, cfg *Config) {
				if cfg.Gateway.RequestTimeout != Duration(120*time.Second) {
					t.Errorf("expected requestTimeout 120s, got %v", cfg.Gateway.RequestTimeout.Duration())
				}
			},
		},
		{
			name: "gateway enabled with requestTimeout 0s defaults to 120s",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: http://localhost:1234
gateway:
  enabled: true
  requestTimeout: 0s
`,
			assert: func(t *testing.T, cfg *Config) {
				if cfg.Gateway.RequestTimeout != Duration(120*time.Second) {
					t.Errorf("expected requestTimeout 120s (zero overridden), got %v", cfg.Gateway.RequestTimeout.Duration())
				}
			},
		},
		{
			name: "gateway enabled with modelsCacheDir",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: http://localhost:1234
gateway:
  enabled: true
  modelsCacheDir: /var/lib/dockmind
`,
			assert: func(t *testing.T, cfg *Config) {
				if cfg.Gateway.ModelsCacheDir != "/var/lib/dockmind" {
					t.Errorf("expected modelsCacheDir /var/lib/dockmind, got %q", cfg.Gateway.ModelsCacheDir)
				}
			},
		},
		{
			name: "gateway enabled with modelsCacheDir absent defaults to empty",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: http://localhost:1234
gateway:
  enabled: true
`,
			assert: func(t *testing.T, cfg *Config) {
				if cfg.Gateway.ModelsCacheDir != "" {
					t.Errorf("expected modelsCacheDir empty, got %q", cfg.Gateway.ModelsCacheDir)
				}
			},
		},
		{
			name: "gateway disabled with modelsCacheDir ok",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
gateway:
  enabled: false
  modelsCacheDir: /var/lib/dockmind
`,
			assert: func(t *testing.T, cfg *Config) {
				if cfg.Gateway.ModelsCacheDir != "/var/lib/dockmind" {
					t.Errorf("expected modelsCacheDir /var/lib/dockmind, got %q", cfg.Gateway.ModelsCacheDir)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(tmp, tc.name+".yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0644); err != nil {
				t.Fatalf("write file: %v", err)
			}

			cfg, err := Load(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.errSub != "" && !strings.Contains(err.Error(), tc.errSub) {
					t.Errorf("expected error to contain %q, got: %v", tc.errSub, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.assert(t, cfg)
		})
	}
}
