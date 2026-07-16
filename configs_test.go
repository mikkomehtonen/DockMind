package dockmind_test

import (
	"strings"
	"testing"

	"github.com/dockmind/dockmind/internal/config"
)

func TestConfigWithGateway(t *testing.T) {
	cfg, err := config.Load("configs/config-with-gateway.yaml")
	if err != nil {
		t.Fatalf("config.Load failed for configs/config-with-gateway.yaml: %v", err)
	}

	if !strings.HasSuffix(cfg.LlamaSwap.HealthURL, "/running") {
		t.Errorf("llamaSwap.healthUrl = %q, want suffix /running", cfg.LlamaSwap.HealthURL)
	}
}
