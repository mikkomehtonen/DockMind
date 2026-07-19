package dockmind_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/dockmind/dockmind/internal/config"
)

func TestREADME(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("failed to read README.md: %v", err)
	}
	readme := string(data)
	lines := strings.Split(readme, "\n")

	cases := []struct {
		name    string
		want    string
		present bool
	}{
		{"links to MVP specification", "docs/DockMind_MVP_Specification.md", true},
		{"links to product overview", "docs/product.md", true},
		{"documents build command", "make build", true},
		{"documents test command", "make test", true},
		{"documents lint command", "make lint", true},
		{"documents --config flag", "--config", true},
		{"documents default config path", "./config.yaml", true},
		{"documents /status route", "/status", true},
		{"documents /power/on route", "/power/on", true},
		{"documents /power/off route", "/power/off", true},
		{"documents /restart route", "/restart", true},
		{"documents /containers/{name}/start route", "/containers/{name}/start", true},
		{"documents /containers/{name}/stop route", "/containers/{name}/stop", true},
		{"documents /health route", "/health", true},
		{"documents /docs route", "/docs", true},
		{"documents web UI route", "web UI", true},
		{"documents favicon route", "/favicon.svg", true},
		{"documents gateway /v1/models route", "/v1/models", true},
		{"documents gateway /v1/chat/completions route", "/v1/chat/completions", true},
		{"documents gateway configuration section", "gateway", true},
		{"documents gateway modelsCacheDir", "modelsCacheDir", true},
		{"documents LOGO_LINK_URL env var", "LOGO_LINK_URL", true},
		{"status example includes state field", "state", true},
		{"status example includes gpuPresent field", "gpuPresent", true},
		{"status example includes gpuName field", "gpuName", true},
		{"status example includes shellyOn field", "shellyOn", true},
		{"status example includes llamaSwapRunning field", "llamaSwapRunning", true},
		{"status example includes llamaSwapHealthy field", "llamaSwapHealthy", true},
		{"status example includes loadedModels field", "loadedModels", true},
		{"status example includes lastError field", "lastError", true},
		{"status example includes cooldownRemaining field", "cooldownRemaining", true},
		{"status example includes idleRemaining field", "idleRemaining", true},
		{"status example includes gpuProcesses field", "gpuProcesses", true},
		{"status example includes usedGpuMemory field", "usedGpuMemory", true},
		{"status example includes gpuMemory field", "gpuMemory", true},
		{"status example includes auxContainers field", "auxContainers", true},
		{"documents cooldown feature", "cooldown", true},
		{"documents idle countdown feature", "idleRemaining", true},
		{"documents auxContainers config section", "auxContainers", true},
		{"does not leak ResultAlreadyInState", "ResultAlreadyInState", false},
		{"does not leak ResultConflict", "ResultConflict", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Contains(readme, c.want)
			if got != c.present {
				t.Errorf("README.md contains %q = %v, want %v", c.want, got, c.present)
			}
		})
	}

	t.Run("no license section", func(t *testing.T) {
		re := regexp.MustCompile(`^#+\s*[Ll]icense`)
		for i, line := range lines {
			if re.MatchString(line) {
				t.Errorf("README.md has a License section on line %d: %q", i+1, line)
			}
		}
	})

	t.Run("yaml config example loads", func(t *testing.T) {
		re := regexp.MustCompile("(?s)```yaml\\n(.*?)\\n```")
		matches := re.FindAllStringSubmatch(readme, -1)
		if len(matches) == 0 {
			t.Fatal("README.md has no fenced yaml config example")
		}

		tmp, err := os.CreateTemp("", "readme-config-*.yaml")
		if err != nil {
			t.Fatalf("failed to create temp file: %v", err)
		}
		defer os.Remove(tmp.Name())

		if _, err := tmp.WriteString(matches[0][1]); err != nil {
			t.Fatalf("failed to write temp config: %v", err)
		}
		if err := tmp.Close(); err != nil {
			t.Fatalf("failed to close temp config: %v", err)
		}

		cfg, err := config.Load(tmp.Name())
		if err != nil {
			t.Fatalf("config.Load failed for README yaml example: %v", err)
		}
		if cfg.Shelly.Address == "" {
			t.Error("shelly.address is empty")
		}
		if cfg.Docker.Container == "" {
			t.Error("docker.container is empty")
		}
		if cfg.LlamaSwap.HealthURL == "" {
			t.Error("llamaSwap.healthUrl is empty")
		}
	})
}
