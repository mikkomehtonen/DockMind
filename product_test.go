package dockmind_test

import (
	"os"
	"strings"
	"testing"
)

func TestProductDoc(t *testing.T) {
	data, err := os.ReadFile("docs/product.md")
	if err != nil {
		t.Fatalf("failed to read docs/product.md: %v", err)
	}
	body := string(data)

	if !strings.Contains(body, "004-web-ui") {
		t.Error("docs/product.md Features list does not reference the 004-web-ui story")
	}
	if !strings.Contains(body, "006-add-favicon-logo") {
		t.Error("docs/product.md Features list does not reference the 006-add-favicon-logo story")
	}
	if !strings.Contains(body, "007-openai-gateway") {
		t.Error("docs/product.md Features list does not reference the 007-openai-gateway story")
	}
	if !strings.Contains(body, "008-openai-gateway") {
		t.Error("docs/product.md Features list does not reference the 008-openai-gateway story")
	}
	if !strings.Contains(body, "010-cache-models-json") {
		t.Error("docs/product.md Features list does not reference the 010-cache-models-json story")
	}
	if !strings.Contains(body, "011-cooldown-protection") {
		t.Error("docs/product.md Features list does not reference the 011-cooldown-protection story")
	}
	if !strings.Contains(body, "012-egpu-unbind-shutdown") {
		t.Error("docs/product.md Features list does not reference the 012-egpu-unbind-shutdown story")
	}
	if !strings.Contains(body, "013-quiet-off-probe-warnings") {
		t.Error("docs/product.md Features list does not reference the 013-quiet-off-probe-warnings story")
	}
	if !strings.Contains(body, "014-llama-swap-running-endpoint") {
		t.Error("docs/product.md Features list does not reference the 014-llama-swap-running-endpoint story")
	}
	if !strings.Contains(body, "015-fix-loaded-models-empty") {
		t.Error("docs/product.md Features list does not reference the 015-fix-loaded-models-empty story")
	}
	if !strings.Contains(body, "016-auto-shutdown-timer") {
		t.Error("docs/product.md Features list does not reference the 016-auto-shutdown-timer story")
	}
	if !strings.Contains(body, "017-gpu-process-guard") {
		t.Error("docs/product.md Features list does not reference the 017-gpu-process-guard story")
	}
	if !strings.Contains(body, "018-optional-containers") {
		t.Error("docs/product.md Features list does not reference the 018-optional-containers story")
	}
	if !strings.Contains(body, "021-aux-containers-gpu-gate") {
		t.Error("docs/product.md Features list does not reference the 021-aux-containers-gpu-gate story")
	}
	if !strings.Contains(body, "023-gpu-utilization-display") {
		t.Error("docs/product.md Features list does not reference the 023-gpu-utilization-display story")
	}
	if strings.Contains(body, "Web UI, Prometheus metrics, or request queuing during startup") {
		t.Error("docs/product.md still lists Web UI as a non-goal")
	}
}
