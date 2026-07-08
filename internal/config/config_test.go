package config

import (
	"os"
	"path/filepath"
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
