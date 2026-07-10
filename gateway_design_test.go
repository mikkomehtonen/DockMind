package dockmind_test

import (
	"os"
	"strings"
	"testing"
)

func TestGatewayDesignDoc(t *testing.T) {
	data, err := os.ReadFile("docs/DockMind_Gateway_Design.md")
	if err != nil {
		t.Fatalf("failed to read docs/DockMind_Gateway_Design.md: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("docs/DockMind_Gateway_Design.md is empty")
	}
	body := strings.ToLower(string(data))

	cases := []struct {
		name string
		want string
	}{
		{"section: High-Level Architecture", "high-level architecture"},
		{"section: Go Package Structure", "go package structure"},
		{"section: State Machine", "state machine"},
		{"section: Startup Sequence", "startup sequence"},
		{"section: Reverse Proxy", "reverse proxy"},
		{"section: Idle Shutdown", "idle shutdown"},
		{"section: Synchronization", "synchronization"},
		{"section: Error Handling", "error handling"},
		{"section: Configuration", "configuration"},
		{"section: Logging", "logging"},
		{"section: Testing Strategy", "testing strategy"},
		{"section: Incremental Implementation Plan", "incremental implementation plan"},
		{"section: Final Review", "final review"},
		{"decision: opt-in", "opt-in"},
		{"decision: gateway.enabled", "gateway.enabled"},
		{"decision: backendUrl", "backendurl"},
		{"decision: idleTimeout", "idletimeout"},
		{"decision: 30m", "30m"},
		{"decision: EnsureReady", "ensureready"},
		{"decision: /v1/", "/v1/"},
		{"decision: power/off", "power/off"},
		{"decision: ReverseProxy", "reverseproxy"},
		{"decision: Milestone", "milestone"},
		{"decision: SSE", "sse"},
		// Design revision additions (review feedback items 1–7).
		{"revision: Rewrite API", "rewrite"},
		{"revision: SetXForwarded", "setxforwarded"},
		{"revision: responseTracker", "responsetracker"},
		{"revision: pendingShutdown", "pendingshutdown"},
		{"revision: /v1/models", "/v1/models"},
		{"revision: startup_timeout", "startup_timeout"},
		{"revision: backend_error", "backend_error"},
		{"revision: grace period", "grace period"},
		{"revision: idle timer initialization", "idle timer initialization"},
		// Model-list cache additions.
		{"revision: model_cache_unavailable", "model_cache_unavailable"},
		{"revision: X-DockMind-Cached", "x-dockmind-cached"},
		{"revision: bufferingWriter", "bufferingwriter"},
		{"revision: atomic.Pointer", "atomic.pointer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(body, c.want) {
				t.Errorf("docs/DockMind_Gateway_Design.md does not contain %q", c.want)
			}
		})
	}
}
