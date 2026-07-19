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
		errSub  string
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
				Shutdown:  ShutdownConfig{Timeout: Duration(30 * time.Second), GPUFreeCheckInterval: Duration(5 * time.Minute)},
				Power:     PowerConfig{Cooldown: Duration(0)},
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
				Shutdown:  ShutdownConfig{Timeout: Duration(30 * time.Second), GPUFreeCheckInterval: Duration(5 * time.Minute)},
				Power:     PowerConfig{Cooldown: Duration(0)},
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
		{
			name: "power cooldown 60s",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
power:
  cooldown: 60s
`,
			want: Config{
				Server:    ServerConfig{Address: ":8080"},
				Shelly:    ShellyConfig{Address: "192.168.1.50", Channel: 0},
				Docker:    DockerConfig{Container: "llama-swap"},
				LlamaSwap: LlamaSwapConfig{HealthURL: "http://localhost:1234/v1/models"},
				GPU:       GPUConfig{PollInterval: Duration(time.Second)},
				Startup:   StartupConfig{Timeout: Duration(60 * time.Second)},
				Shutdown:  ShutdownConfig{Timeout: Duration(30 * time.Second), GPUFreeCheckInterval: Duration(5 * time.Minute)},
				Power:     PowerConfig{Cooldown: Duration(60 * time.Second)},
			},
		},
		{
			name: "power section absent defaults to zero",
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
				Shutdown:  ShutdownConfig{Timeout: Duration(30 * time.Second), GPUFreeCheckInterval: Duration(5 * time.Minute)},
				Power:     PowerConfig{Cooldown: Duration(0)},
			},
		},
		{
			name: "power cooldown 0s",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
power:
  cooldown: 0s
`,
			want: Config{
				Server:    ServerConfig{Address: ":8080"},
				Shelly:    ShellyConfig{Address: "192.168.1.50", Channel: 0},
				Docker:    DockerConfig{Container: "llama-swap"},
				LlamaSwap: LlamaSwapConfig{HealthURL: "http://localhost:1234/v1/models"},
				GPU:       GPUConfig{PollInterval: Duration(time.Second)},
				Startup:   StartupConfig{Timeout: Duration(60 * time.Second)},
				Shutdown:  ShutdownConfig{Timeout: Duration(30 * time.Second), GPUFreeCheckInterval: Duration(5 * time.Minute)},
				Power:     PowerConfig{Cooldown: Duration(0)},
			},
		},
		{
			name: "negative power cooldown fails",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
power:
  cooldown: -1s
`,
			wantErr: true,
			errSub:  "power.cooldown",
		},
		{
			name: "gpuFreeCheckInterval absent defaults to 5m",
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
				Shutdown:  ShutdownConfig{Timeout: Duration(30 * time.Second), GPUFreeCheckInterval: Duration(5 * time.Minute)},
				Power:     PowerConfig{Cooldown: Duration(0)},
			},
		},
		{
			name: "gpuFreeCheckInterval 2m",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
shutdown:
  gpuFreeCheckInterval: 2m
`,
			want: Config{
				Server:    ServerConfig{Address: ":8080"},
				Shelly:    ShellyConfig{Address: "192.168.1.50", Channel: 0},
				Docker:    DockerConfig{Container: "llama-swap"},
				LlamaSwap: LlamaSwapConfig{HealthURL: "http://localhost:1234/v1/models"},
				GPU:       GPUConfig{PollInterval: Duration(time.Second)},
				Startup:   StartupConfig{Timeout: Duration(60 * time.Second)},
				Shutdown:  ShutdownConfig{Timeout: Duration(30 * time.Second), GPUFreeCheckInterval: Duration(2 * time.Minute)},
				Power:     PowerConfig{Cooldown: Duration(0)},
			},
		},
		{
			name: "gpuFreeCheckInterval 0s defaults to 5m",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
shutdown:
  gpuFreeCheckInterval: 0s
`,
			want: Config{
				Server:    ServerConfig{Address: ":8080"},
				Shelly:    ShellyConfig{Address: "192.168.1.50", Channel: 0},
				Docker:    DockerConfig{Container: "llama-swap"},
				LlamaSwap: LlamaSwapConfig{HealthURL: "http://localhost:1234/v1/models"},
				GPU:       GPUConfig{PollInterval: Duration(time.Second)},
				Startup:   StartupConfig{Timeout: Duration(60 * time.Second)},
				Shutdown:  ShutdownConfig{Timeout: Duration(30 * time.Second), GPUFreeCheckInterval: Duration(5 * time.Minute)},
				Power:     PowerConfig{Cooldown: Duration(0)},
			},
		},
		{
			name: "negative gpuFreeCheckInterval fails",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
shutdown:
  gpuFreeCheckInterval: -1s
`,
			wantErr: true,
			errSub:  "gpuFreeCheckInterval",
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
				if tc.errSub != "" && !strings.Contains(err.Error(), tc.errSub) {
					t.Errorf("expected error to contain %q, got: %v", tc.errSub, err)
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

func TestEffectiveIdleTimeout(t *testing.T) {
	cases := []struct {
		name         string
		idleTimeout  time.Duration
		cooldown     time.Duration
		want         time.Duration
		wantAdjusted bool
	}{
		{"idleTimeout less than cooldown", 30 * time.Second, 60 * time.Second, 60 * time.Second, true},
		{"idleTimeout equal to cooldown", 60 * time.Second, 60 * time.Second, 60 * time.Second, false},
		{"idleTimeout greater than cooldown", 120 * time.Second, 60 * time.Second, 120 * time.Second, false},
		{"idleTimeout zero (disabled)", 0, 60 * time.Second, 0, false},
		{"cooldown zero (disabled)", 30 * time.Second, 0, 30 * time.Second, false},
		{"both zero", 0, 0, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, adjusted := EffectiveIdleTimeout(tc.idleTimeout, tc.cooldown)
			if got != tc.want {
				t.Errorf("EffectiveIdleTimeout(%v, %v) = %v, want %v", tc.idleTimeout, tc.cooldown, got, tc.want)
			}
			if adjusted != tc.wantAdjusted {
				t.Errorf("EffectiveIdleTimeout(%v, %v) adjusted = %v, want %v", tc.idleTimeout, tc.cooldown, adjusted, tc.wantAdjusted)
			}
		})
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
		{
			name: "gateway enabled with modelsRefreshInterval absent defaults to 60s",
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
				if cfg.Gateway.ModelsRefreshInterval != Duration(60*time.Second) {
					t.Errorf("expected modelsRefreshInterval 60s, got %v", cfg.Gateway.ModelsRefreshInterval.Duration())
				}
			},
		},
		{
			name: "gateway enabled with modelsRefreshInterval 30s",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: http://localhost:1234
gateway:
  enabled: true
  modelsRefreshInterval: 30s
`,
			assert: func(t *testing.T, cfg *Config) {
				if cfg.Gateway.ModelsRefreshInterval != Duration(30*time.Second) {
					t.Errorf("expected modelsRefreshInterval 30s, got %v", cfg.Gateway.ModelsRefreshInterval.Duration())
				}
			},
		},
		{
			name: "gateway enabled with negative modelsRefreshInterval fails",
			content: `shelly:
  address: 192.168.1.50
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: http://localhost:1234
gateway:
  enabled: true
  modelsRefreshInterval: -1s
`,
			wantErr: true,
			errSub:  "modelsRefreshInterval",
		},
		{
			name: "gateway disabled with modelsRefreshInterval absent defaults to 0",
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
				if cfg.Gateway.ModelsRefreshInterval != Duration(0) {
					t.Errorf("expected modelsRefreshInterval 0, got %v", cfg.Gateway.ModelsRefreshInterval.Duration())
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
