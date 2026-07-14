package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

type ServerConfig struct {
	Address string `yaml:"address"`
}

type ShellyConfig struct {
	Address string `yaml:"address"`
	Channel int    `yaml:"channel"`
}

type DockerConfig struct {
	Container string `yaml:"container"`
}

type LlamaSwapConfig struct {
	HealthURL  string `yaml:"healthUrl"`
	BackendURL string `yaml:"backendUrl"`
}

type GatewayConfig struct {
	Enabled        bool     `yaml:"enabled"`
	IdleTimeout    Duration `yaml:"idleTimeout"`
	RequestTimeout Duration `yaml:"requestTimeout"`
	ModelsCacheDir string   `yaml:"modelsCacheDir"`
}

type GPUConfig struct {
	PollInterval Duration `yaml:"pollInterval"`
}

type StartupConfig struct {
	Timeout Duration `yaml:"timeout"`
}

type ShutdownConfig struct {
	Timeout Duration `yaml:"timeout"`
}

type PowerConfig struct {
	Cooldown Duration `yaml:"cooldown"`
}

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Shelly    ShellyConfig    `yaml:"shelly"`
	Docker    DockerConfig    `yaml:"docker"`
	LlamaSwap LlamaSwapConfig `yaml:"llamaSwap"`
	GPU       GPUConfig       `yaml:"gpu"`
	Startup   StartupConfig   `yaml:"startup"`
	Shutdown  ShutdownConfig  `yaml:"shutdown"`
	Gateway   GatewayConfig   `yaml:"gateway"`
	Power     PowerConfig     `yaml:"power"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Address == "" {
		cfg.Server.Address = ":8080"
	}
	if cfg.GPU.PollInterval == 0 {
		cfg.GPU.PollInterval = Duration(time.Second)
	}
	if cfg.Startup.Timeout == 0 {
		cfg.Startup.Timeout = Duration(60 * time.Second)
	}
	if cfg.Shutdown.Timeout == 0 {
		cfg.Shutdown.Timeout = Duration(30 * time.Second)
	}
	if cfg.Gateway.Enabled && cfg.Gateway.RequestTimeout == 0 {
		cfg.Gateway.RequestTimeout = Duration(120 * time.Second)
	}
}

func validate(cfg *Config) error {
	if cfg.Shelly.Address == "" {
		return errors.New("shelly.address is required")
	}
	if cfg.Docker.Container == "" {
		return errors.New("docker.container is required")
	}
	if cfg.LlamaSwap.HealthURL == "" {
		return errors.New("llamaSwap.healthUrl is required")
	}
	if cfg.GPU.PollInterval <= 0 {
		return errors.New("gpu.pollInterval must be positive")
	}
	if cfg.Startup.Timeout <= 0 {
		return errors.New("startup.timeout must be positive")
	}
	if cfg.Shutdown.Timeout <= 0 {
		return errors.New("shutdown.timeout must be positive")
	}
	if cfg.Power.Cooldown < 0 {
		return errors.New("power.cooldown must be >= 0")
	}
	if cfg.Gateway.Enabled {
		if cfg.LlamaSwap.BackendURL == "" {
			return errors.New("llamaSwap.backendUrl is required when gateway.enabled is true")
		}
		parsed, err := url.Parse(cfg.LlamaSwap.BackendURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("llamaSwap.backendUrl must be a valid URL with scheme and host: %q", cfg.LlamaSwap.BackendURL)
		}
		if cfg.Gateway.IdleTimeout < 0 {
			return errors.New("gateway.idleTimeout must be >= 0")
		}
		if cfg.Gateway.RequestTimeout <= 0 {
			return errors.New("gateway.requestTimeout must be positive")
		}
	}
	return nil
}

// EffectiveIdleTimeout returns the idle timeout adjusted for cooldown.
// When idleTimeout > 0 and cooldown > idleTimeout, the effective idle
// timeout is raised to cooldown (the minimum sensible value). Returns
// the effective duration and whether it was adjusted.
func EffectiveIdleTimeout(idleTimeout, cooldown time.Duration) (time.Duration, bool) {
	if idleTimeout > 0 && cooldown > idleTimeout {
		return cooldown, true
	}
	return idleTimeout, false
}
