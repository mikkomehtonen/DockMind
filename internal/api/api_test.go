package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dockmind/dockmind/internal/state"
)

type fakeStateMachine struct {
	status   state.StatusResponse
	powerOn  state.PowerResult
	powerOff state.PowerResult
	restart  state.PowerResult
}

func (f *fakeStateMachine) Status() state.StatusResponse {
	return f.status
}

func (f *fakeStateMachine) PowerOn() state.PowerResult {
	return f.powerOn
}

func (f *fakeStateMachine) PowerOff() state.PowerResult {
	return f.powerOff
}

func (f *fakeStateMachine) Restart() state.PowerResult {
	return f.restart
}

func TestRoutes(t *testing.T) {
	cases := []struct {
		name          string
		method        string
		path          string
		setup         func(*fakeStateMachine)
		wantStatus    int
		wantBody      string
		wantEmptyBody bool
	}{
		{
			name:   "GET /status ready",
			method: http.MethodGet,
			path:   "/status",
			setup: func(f *fakeStateMachine) {
				f.status = state.StatusResponse{State: "Ready"}
			},
			wantStatus: http.StatusOK,
			wantBody:   `"state":"Ready"`,
		},
		{
			name:   "GET /status off",
			method: http.MethodGet,
			path:   "/status",
			setup: func(f *fakeStateMachine) {
				f.status = state.StatusResponse{State: "Off"}
			},
			wantStatus: http.StatusOK,
			wantBody:   `"state":"Off"`,
		},
		{
			name:   "POST /power/on accepted",
			method: http.MethodPost,
			path:   "/power/on",
			setup: func(f *fakeStateMachine) {
				f.powerOn = state.ResultAccepted
			},
			wantStatus: http.StatusAccepted,
		},
		{
			name:   "POST /power/on already in state",
			method: http.MethodPost,
			path:   "/power/on",
			setup: func(f *fakeStateMachine) {
				f.powerOn = state.ResultAlreadyInState
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "POST /power/on conflict",
			method: http.MethodPost,
			path:   "/power/on",
			setup: func(f *fakeStateMachine) {
				f.powerOn = state.ResultConflict
			},
			wantStatus: http.StatusConflict,
		},
		{
			name:   "POST /power/off accepted",
			method: http.MethodPost,
			path:   "/power/off",
			setup: func(f *fakeStateMachine) {
				f.powerOff = state.ResultAccepted
			},
			wantStatus: http.StatusAccepted,
		},
		{
			name:   "POST /power/off already in state",
			method: http.MethodPost,
			path:   "/power/off",
			setup: func(f *fakeStateMachine) {
				f.powerOff = state.ResultAlreadyInState
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "POST /restart accepted",
			method: http.MethodPost,
			path:   "/restart",
			setup: func(f *fakeStateMachine) {
				f.restart = state.ResultAccepted
			},
			wantStatus: http.StatusAccepted,
		},
		{
			name:   "POST /restart conflict",
			method: http.MethodPost,
			path:   "/restart",
			setup: func(f *fakeStateMachine) {
				f.restart = state.ResultConflict
			},
			wantStatus: http.StatusConflict,
		},
		{
			name:          "GET /health",
			method:        http.MethodGet,
			path:          "/health",
			wantStatus:    http.StatusOK,
			wantEmptyBody: true,
		},
		{
			name:       "GET /foo unknown",
			method:     http.MethodGet,
			path:       "/foo",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "GET /power/on wrong method",
			method:     http.MethodGet,
			path:       "/power/on",
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "GET /power/off wrong method",
			method:     http.MethodGet,
			path:       "/power/off",
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "POST /docs wrong method",
			method:     http.MethodPost,
			path:       "/docs",
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "POST /openapi.json wrong method",
			method:     http.MethodPost,
			path:       "/openapi.json",
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeStateMachine{}
			if tc.setup != nil {
				tc.setup(fake)
			}
			server := NewServer(fake, nil)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d", tc.wantStatus, rec.Code)
			}
			if tc.wantEmptyBody && rec.Body.String() != "" {
				t.Fatalf("expected empty body, got %q", rec.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("expected body to contain %q, got %q", tc.wantBody, rec.Body.String())
			}
		})
	}
}

func TestSwaggerRoutes(t *testing.T) {
	server := NewServer(&fakeStateMachine{}, nil)

	t.Run("GET /docs", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/docs", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Fatalf("expected Content-Type to contain text/html, got %q", ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "swagger-ui") {
			t.Fatalf("expected body to contain swagger-ui, got %q", body)
		}
		if !strings.Contains(body, "/openapi.json") {
			t.Fatalf("expected body to contain /openapi.json, got %q", body)
		}
	})

	t.Run("GET /openapi.json", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "application/json") {
			t.Fatalf("expected Content-Type to contain application/json, got %q", ct)
		}

		var spec map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
			t.Fatalf("expected body to parse as JSON: %v", err)
		}
		if spec["openapi"] != "3.0.3" {
			t.Fatalf("expected openapi version 3.0.3, got %v", spec["openapi"])
		}

		paths, ok := spec["paths"].(map[string]any)
		if !ok {
			t.Fatalf("expected paths to be an object")
		}
		for _, p := range []string{"/status", "/power/on", "/power/off", "/restart", "/health", "/v1/models", "/v1/chat/completions"} {
			if _, ok := paths[p]; !ok {
				t.Fatalf("expected paths to contain %q", p)
			}
		}

		components, ok := spec["components"].(map[string]any)
		if !ok {
			t.Fatalf("expected components to be an object")
		}
		schemas, ok := components["schemas"].(map[string]any)
		if !ok {
			t.Fatalf("expected components.schemas to be an object")
		}
		statusResponse, ok := schemas["StatusResponse"].(map[string]any)
		if !ok {
			t.Fatalf("expected components.schemas.StatusResponse to be an object")
		}
		properties, ok := statusResponse["properties"].(map[string]any)
		if !ok {
			t.Fatalf("expected components.schemas.StatusResponse.properties to be an object")
		}
		for _, field := range []string{"state", "gpuPresent", "gpuName", "shellyOn", "llamaSwapRunning", "llamaSwapHealthy", "lastError"} {
			if _, ok := properties[field]; !ok {
				t.Fatalf("expected StatusResponse properties to contain %q", field)
			}
		}

		openAIError, ok := schemas["OpenAIError"].(map[string]any)
		if !ok {
			t.Fatalf("expected components.schemas.OpenAIError to be an object")
		}
		errorProp, ok := openAIError["properties"].(map[string]any)
		if !ok {
			t.Fatalf("expected OpenAIError.properties to be an object")
		}
		errorObj, ok := errorProp["error"].(map[string]any)
		if !ok {
			t.Fatalf("expected OpenAIError.properties.error to be an object")
		}
		errorProps, ok := errorObj["properties"].(map[string]any)
		if !ok {
			t.Fatalf("expected OpenAIError.properties.error.properties to be an object")
		}
		for _, field := range []string{"message", "type", "code"} {
			if _, ok := errorProps[field]; !ok {
				t.Fatalf("expected OpenAIError error properties to contain %q", field)
			}
		}
	})

}

func TestWebUIRoutes(t *testing.T) {
	t.Setenv("LOGO_LINK_URL", "")
	server := NewServer(&fakeStateMachine{}, nil)

	t.Run("GET /favicon.svg", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/favicon.svg", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "image/svg+xml") {
			t.Fatalf("expected Content-Type to contain image/svg+xml, got %q", ct)
		}
		if !strings.Contains(rec.Body.String(), "<svg") {
			t.Fatalf("expected body to contain <svg")
		}
	})

	t.Run("POST /favicon.svg wrong method", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/favicon.svg", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
		}
	})

	t.Run("GET /", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Fatalf("expected Content-Type to contain text/html, got %q", ct)
		}
		body := rec.Body.String()
		for _, want := range []string{
			"DockMind",
			"/status",
			"/power/on",
			"/power/off",
			"/restart",
			"/docs",
			"fetch",
			"setInterval",
			"llama-swap health",
			"component__dot.is-danger",
			"/favicon.svg",
			"app__logo",
			`rel="icon"`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("expected body to contain %q, got %q", want, body)
			}
		}
		if strings.Contains(body, "https://") {
			t.Fatalf("expected body to contain no https:// URLs")
		}
		if strings.Contains(body, "http://") {
			t.Fatalf("expected body to contain no http:// URLs")
		}
		if strings.Contains(body, `class="app__logo-link"`) {
			t.Fatalf("expected body to NOT contain app__logo-link link element when LOGO_LINK_URL is unset")
		}
		if strings.Contains(body, "Health check") {
			t.Fatalf("expected body to no longer contain the confusing label \"Health check\", got %q", body)
		}
	})

	t.Run("POST / wrong method", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
		}
	})

	t.Run("GET /foo unknown path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/foo", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
		}
	})

	t.Run("regression GET /status", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"state"`) {
			t.Fatalf("expected body to contain \"state\", got %q", rec.Body.String())
		}
	})

	t.Run("regression GET /health", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
		}
		if rec.Body.String() != "" {
			t.Fatalf("expected empty body, got %q", rec.Body.String())
		}
	})

	t.Run("regression GET /docs", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/docs", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "swagger-ui") {
			t.Fatalf("expected body to contain swagger-ui, got %q", rec.Body.String())
		}
	})
}

func TestGatewayRoutes(t *testing.T) {
	t.Run("no handlers returns 404", func(t *testing.T) {
		server := NewServer(&fakeStateMachine{}, nil)
		for _, path := range []string{"/v1/models", "/v1/chat/completions"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("expected 404 for %s without gateway handlers, got %d", path, rec.Code)
			}
		}
	})

	t.Run("handlers are invoked", func(t *testing.T) {
		var modelsCalled, inferenceCalled bool
		modelsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			modelsCalled = true
			w.WriteHeader(http.StatusOK)
		})
		inferenceHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inferenceCalled = true
			w.WriteHeader(http.StatusOK)
		})

		server := NewServer(&fakeStateMachine{}, nil)
		server.SetGatewayHandlers(inferenceHandler, modelsHandler)

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 for /v1/models, got %d", rec.Code)
		}
		if !modelsCalled {
			t.Error("models handler was not invoked")
		}

		req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec = httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 for /v1/chat/completions, got %d", rec.Code)
		}
		if !inferenceCalled {
			t.Error("inference handler was not invoked")
		}
	})

	t.Run("existing routes still work with gateway handlers", func(t *testing.T) {
		server := NewServer(&fakeStateMachine{}, nil)
		server.SetGatewayHandlers(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		)

		cases := []struct {
			method string
			path   string
			want   int
		}{
			{http.MethodGet, "/status", http.StatusOK},
			{http.MethodPost, "/power/on", http.StatusAccepted},
			{http.MethodPost, "/power/off", http.StatusAccepted},
			{http.MethodPost, "/restart", http.StatusAccepted},
			{http.MethodGet, "/health", http.StatusOK},
			{http.MethodGet, "/docs", http.StatusOK},
			{http.MethodGet, "/", http.StatusOK},
		}
		for _, tc := range cases {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("expected %d for %s %s, got %d", tc.want, tc.method, tc.path, rec.Code)
			}
		}
	})
}

func TestLogoLink(t *testing.T) {
	t.Setenv("LOGO_LINK_URL", "https://example.com")
	server := NewServer(&fakeStateMachine{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="app__logo-link"`) {
		t.Fatalf("expected body to contain app__logo-link link element when LOGO_LINK_URL is set")
	}
	if !strings.Contains(body, `href="https://example.com"`) {
		t.Fatalf("expected body to contain href=\"https://example.com\"")
	}
}
