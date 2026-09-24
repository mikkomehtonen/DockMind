package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dockmind/dockmind/internal/api"
	"github.com/dockmind/dockmind/internal/config"
	"github.com/dockmind/dockmind/internal/gateway"
	"github.com/dockmind/dockmind/internal/state"
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

// TestWireGateway verifies, by behavior, that wireGateway connects all three
// gateway seams on the API server: the OpenAI-compatible routes, live idle
// status reporting, and the runtime idle auto-shutdown toggle.
func TestWireGateway(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer backend.Close()

	gw, err := gateway.NewGateway(backend.URL, time.Minute, time.Second, &testStateController{st: state.Ready}, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	server := api.NewServer(&testAPIStateMachine{}, slog.Default())
	wireGateway(server, gw)

	t.Run("gateway routes are served", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 from wired gateway route, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("idle status is reported", func(t *testing.T) {
		var status state.StatusResponse
		if err := getJSON(t, server, "/status", &status); err != nil {
			t.Fatal(err)
		}
		if status.IdleRemaining <= 0 {
			t.Errorf("expected idleRemaining > 0 from wired idle reporter, got %v", status.IdleRemaining)
		}
	})

	t.Run("idle shutdown toggle is wired", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/idle-shutdown/disable", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 from wired toggle endpoint, got %d", rec.Code)
		}

		var status state.StatusResponse
		if err := getJSON(t, server, "/status", &status); err != nil {
			t.Fatal(err)
		}
		if !status.IdleShutdownAvailable {
			t.Error("expected idleShutdownAvailable true from wired toggle controller")
		}
		if status.IdleShutdownEnabled {
			t.Error("expected idleShutdownEnabled false after disabling via wired toggle")
		}
	})
}

func getJSON(t *testing.T, server *api.Server, path string, out any) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected GET %s to return 200, got %d: %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		return err
	}
	return nil
}

// testStateController is a minimal gateway.StateController: the machine is
// stuck in the given state and never blocks idle shutdown.
type testStateController struct {
	st state.State
}

func (c *testStateController) State() state.State                    { return c.st }
func (c *testStateController) PowerOff() state.PowerResult           { return state.ResultConflict }
func (c *testStateController) EnsureReady(ctx context.Context) error { return nil }
func (c *testStateController) IdleShutdownBlocked() bool             { return false }

// testAPIStateMachine is a minimal api.StateMachine: status reports Ready and
// every mutating call conflicts (none are exercised by TestWireGateway).
type testAPIStateMachine struct{}

func (m *testAPIStateMachine) Status() state.StatusResponse {
	return state.StatusResponse{State: "Ready"}
}
func (m *testAPIStateMachine) PowerOn() state.PowerResult  { return state.ResultConflict }
func (m *testAPIStateMachine) PowerOff() state.PowerResult { return state.ResultConflict }
func (m *testAPIStateMachine) Restart() state.PowerResult  { return state.ResultConflict }
func (m *testAPIStateMachine) StartAuxContainer(string) state.AuxResult {
	return state.AuxResultConflict
}
func (m *testAPIStateMachine) StopAuxContainer(string) state.AuxResult {
	return state.AuxResultConflict
}
