package config

import (
	"errors"
	"fmt"
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
	HealthURL string `yaml:"healthUrl"`
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

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Shelly    ShellyConfig    `yaml:"shelly"`
	Docker    DockerConfig    `yaml:"docker"`
	LlamaSwap LlamaSwapConfig `yaml:"llamaSwap"`
	GPU       GPUConfig       `yaml:"gpu"`
	Startup   StartupConfig   `yaml:"startup"`
	Shutdown  ShutdownConfig  `yaml:"shutdown"`
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
	return nil
}
