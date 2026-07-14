package gateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockmind/dockmind/internal/state"
)

// fakeController implements StateController for testing.
type fakeController struct {
	mu               sync.Mutex
	currentState     state.State
	changeCh         chan struct{}
	powerOffResult   state.PowerResult
	powerOffCalls    int
	ensureReadyErr   error // nil = success
	ensureReadyCalls int
	autoStart        bool // when true, EnsureReady transitions Off→Ready
}

func newFakeController() *fakeController {
	return &fakeController{
		currentState:   state.Ready,
		changeCh:       make(chan struct{}),
		powerOffResult: state.ResultAccepted,
		autoStart:      true,
	}
}

func (f *fakeController) setState(s state.State, err error) {
	f.mu.Lock()
	old := f.currentState
	f.currentState = s
	if old != s && f.changeCh != nil {
		close(f.changeCh)
	}
	f.changeCh = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeController) State() state.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.currentState
}

func (f *fakeController) PowerOff() state.PowerResult {
	f.mu.Lock()
	f.powerOffCalls++
	result := f.powerOffResult
	if f.currentState == state.Ready {
		f.currentState = state.Off
	}
	f.mu.Unlock()
	return result
}

func (f *fakeController) EnsureReady(ctx context.Context) error {
	f.mu.Lock()
	f.ensureReadyCalls++
	f.mu.Unlock()
	if f.ensureReadyErr != nil {
		return f.ensureReadyErr
	}

	for {
		f.mu.Lock()
		current := f.currentState
		ch := f.changeCh
		f.mu.Unlock()

		switch current {
		case state.Ready:
			return nil
		case state.Off, state.Starting, state.ShuttingDown:
			if f.autoStart {
				f.setState(state.Ready, nil)
				return nil
			}
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		default: // Error
			return errors.New("backend in error state")
		}
	}
}

func TestReverseProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"ok"}`))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"result":"ok"`) {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

func TestReverseProxy_PathRewrite(t *testing.T) {
	var receivedPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Write([]byte(`{}`))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if receivedPath != "/v1/chat/completions" {
		t.Errorf("expected backend path /v1/chat/completions, got %s", receivedPath)
	}
}

func TestReverseProxy_BadBackend(t *testing.T) {
	ctrl := newFakeController()
	gw, err := NewGateway("http://localhost:99999", 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for unreachable backend, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "proxy_error") {
		t.Errorf("expected body to contain proxy_error, got %s", rec.Body.String())
	}
}

func TestReverseProxy_HeadersForwarded(t *testing.T) {
	var receivedHeader string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeader = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if receivedHeader != "Bearer test-token" {
		t.Errorf("expected Authorization header forwarded, got %q", receivedHeader)
	}
}

func TestReverseProxy_ResponseHeadersCopied(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom-Header", "test-value")
		w.Write([]byte(`{}`))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if rec.Header().Get("X-Custom-Header") != "test-value" {
		t.Errorf("expected X-Custom-Header copied from backend, got %q", rec.Header().Get("X-Custom-Header"))
	}
}

func TestModelsHandler_CacheAndServe(t *testing.T) {
	modelResponse := `{"data":[{"id":"gpt-4","object":"model"},{"id":"gpt-3.5-turbo","object":"model"}]}`

	var callCount int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(modelResponse))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	// First call via ModelsHandler.
	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec1 := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec1.Code, rec1.Body.String())
	}
	if !strings.Contains(rec1.Body.String(), "gpt-4") {
		t.Errorf("unexpected body: %s", rec1.Body.String())
	}

	// Verify cache was populated.
	cached := gw.cachedModels.Load()
	if cached == nil || len(cached.body) == 0 {
		t.Error("cache should be populated after successful fetch")
	}

	// Second call — still proxies to backend when Ready.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec2 := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestModelsHandler_CacheServedWhenOff(t *testing.T) {
	ctrl := newFakeController()
	ctrl.setState(state.Off, nil)

	gw, err := NewGateway("http://localhost:99999", 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	// Capture lastActivity before request.
	gw.activeMu.Lock()
	lastActivityBefore := gw.lastActivity
	gw.activeMu.Unlock()

	// First call with no cache: should return 503.
	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec1 := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", rec1.Code, rec1.Body.String())
	}
	body1 := rec1.Body.String()
	if !strings.Contains(body1, "model_cache_unavailable") {
		t.Errorf("expected model_cache_unavailable in body, got %s", body1)
	}

	// Manually populate cache.
	cachedData := []byte(`{"data":[{"id":"cached-model","object":"model"}]}`)
	gw.cachedModels.Store(&modelCache{body: cachedData, contentType: "application/json"})

	// Second call with cache: should serve from cache.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec2 := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 with cached data, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "cached-model") {
		t.Errorf("unexpected body: %s", rec2.Body.String())
	}
	if rec2.Header().Get("X-DockMind-Cached") != "true" {
		t.Error("expected X-DockMind-Cached header to be true")
	}

	ctrl.mu.Lock()
	ensureCalls := ctrl.ensureReadyCalls
	ctrl.mu.Unlock()
	if ensureCalls != 0 {
		t.Errorf("expected EnsureReady not called for cached/off models request, got %d calls", ensureCalls)
	}

	gw.activeMu.Lock()
	active := gw.active
	lastActivityAfter := gw.lastActivity
	gw.activeMu.Unlock()
	if active != 0 {
		t.Errorf("expected active=0 after cached models request, got %d", active)
	}
	if !lastActivityAfter.Equal(lastActivityBefore) {
		t.Errorf("expected lastActivity unchanged for cached models request")
	}
}

func TestModelsHandler_NoCacheWhenError(t *testing.T) {
	ctrl := newFakeController()
	ctrl.setState(state.Error, errors.New("gpu timeout"))

	gw, err := NewGateway("http://localhost:99999", 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	// Populate cache.
	cachedData := []byte(`{"data":[{"id":"cached-model","object":"model"}]}`)
	gw.cachedModels.Store(&modelCache{body: cachedData, contentType: "application/json"})

	// Request — should return 503 backend_error, NOT serve cache.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for error state (no cache serve), got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "backend_error") {
		t.Errorf("expected backend_error in body, got %s", body)
	}
}

func TestModelsHandler_CacheUpdatedOnSuccess(t *testing.T) {
	var response string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(response))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	response = `{"data":[{"id":"model-a","object":"model"}]}`
	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec1 := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec1.Code)
	}

	response = `{"data":[{"id":"model-b","object":"model"}]}`
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec2 := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}

	cached := gw.cachedModels.Load()
	if cached == nil || !strings.Contains(string(cached.body), "model-b") {
		t.Errorf("cache not updated with latest data: %v", cached)
	}
}

func TestAutoStart(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	ctrl.setState(state.Off, nil)

	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (auto-started), got %d: %s", rec.Code, rec.Body.String())
	}
	if ctrl.State() != state.Ready {
		t.Errorf("expected state Ready after auto-start, got %s", ctrl.State())
	}
}

func TestAutoStart_BackendError(t *testing.T) {
	ctrl := newFakeController()
	ctrl.ensureReadyErr = state.ErrBackendError

	gw, err := NewGateway("http://localhost:99999", 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for error state, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "backend_error") {
		t.Errorf("expected backend_error in body, got %s", body)
	}
}

func TestAutoStart_StartupTimeout(t *testing.T) {
	ctrl := newFakeController()
	ctrl.ensureReadyErr = context.DeadlineExceeded

	gw, err := NewGateway("http://localhost:99999", 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for startup timeout, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "startup_timeout") {
		t.Errorf("expected startup_timeout in body, got %s", body)
	}
}

func TestAutoStart_ClientCanceled_NoResponse(t *testing.T) {
	ctrl := newFakeController()
	ctrl.ensureReadyErr = context.Canceled

	gw, err := NewGateway("http://localhost:99999", 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	// No response should be written for canceled context.
	if rec.Code != http.StatusOK {
		t.Errorf("expected no explicit write (200 default), got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() > 0 {
		t.Errorf("expected empty body for canceled context, got: %s", rec.Body.String())
	}
}

func TestIdleShutdown(t *testing.T) {
	ctrl := newFakeController()
	gw, err := NewGatewayWithPollInterval(
		"http://localhost:99999", 50*time.Millisecond, 2*time.Second,
		10*time.Millisecond, ctrl, slog.Default(),
	)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.StartIdleWatcher(context.Background())
	defer gw.StopIdleWatcher()

	// Simulate an active request to set lastActivity.
	gw.activeMu.Lock()
	gw.lastActivity = time.Now().Add(-100 * time.Millisecond) // Pretend activity was 100ms ago
	gw.activeMu.Unlock()

	// Wait for idle timeout + grace period ticks.
	time.Sleep(200 * time.Millisecond)

	ctrl.mu.Lock()
	calls := ctrl.powerOffCalls
	ctrl.mu.Unlock()
	if calls == 0 {
		t.Error("expected PowerOff to be called after idle timeout")
	}
}

func TestIdleShutdown_Cooldown(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctrl := newFakeController()
	ctrl.powerOffResult = state.ResultCooldown
	gw, err := NewGatewayWithPollInterval(
		"http://localhost:99999", 50*time.Millisecond, 2*time.Second,
		10*time.Millisecond, ctrl, logger,
	)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.StartIdleWatcher(context.Background())
	defer gw.StopIdleWatcher()

	gw.activeMu.Lock()
	gw.lastActivity = time.Now().Add(-100 * time.Millisecond)
	gw.activeMu.Unlock()

	time.Sleep(200 * time.Millisecond)

	gw.activeMu.Lock()
	pending := gw.pendingShutdown
	gw.activeMu.Unlock()
	if pending {
		t.Error("expected pendingShutdown to be false after ResultCooldown")
	}

	logOut := buf.String()
	if !strings.Contains(logOut, "cooldown") {
		t.Errorf("expected debug log containing 'cooldown', got: %s", logOut)
	}
}

func TestIdleShutdown_Disabled(t *testing.T) {
	ctrl := newFakeController()
	gw, err := NewGatewayWithPollInterval(
		"http://localhost:99999", 0, 2*time.Second,
		10*time.Millisecond, ctrl, slog.Default(),
	)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.StartIdleWatcher(context.Background())
	defer gw.StopIdleWatcher()

	time.Sleep(50 * time.Millisecond)

	ctrl.mu.Lock()
	calls := ctrl.powerOffCalls
	ctrl.mu.Unlock()
	if calls > 0 {
		t.Error("expected no PowerOff when idleTimeout is 0")
	}
}

func TestIdleShutdown_ClearedByRequest(t *testing.T) {
	ctrl := newFakeController()
	gw, err := NewGatewayWithPollInterval(
		"http://localhost:99999", 50*time.Millisecond, 2*time.Second,
		10*time.Millisecond, ctrl, slog.Default(),
	)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.StartIdleWatcher(context.Background())
	defer gw.StopIdleWatcher()

	// First set pendingShutdown manually.
	gw.activeMu.Lock()
	gw.lastActivity = time.Now().Add(-100 * time.Millisecond) // Old activity
	gw.pendingShutdown = true
	gw.activeMu.Unlock()

	time.Sleep(25 * time.Millisecond) // Enough for a tick to confirm shutdown

	ctrl.mu.Lock()
	callsBefore := ctrl.powerOffCalls
	ctrl.mu.Unlock()

	// New request clears pendingShutdown.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer backend.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	ctrl.mu.Lock()
	callsAfter := ctrl.powerOffCalls
	ctrl.mu.Unlock()

	// No new PowerOff calls should occur after the request clears pendingShutdown.
	if callsAfter != callsBefore {
		t.Errorf("expected pendingShutdown to be cleared by new request (calls before=%d after=%d)", callsBefore, callsAfter)
	}
}

func TestIdleWatcher_WithContext(t *testing.T) {
	ctrl := newFakeController()
	gw, err := NewGatewayWithPollInterval(
		"http://localhost:99999", 50*time.Millisecond, 2*time.Second,
		10*time.Millisecond, ctrl, slog.Default(),
	)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	gw.StartIdleWatcher(ctx)
	cancel() // Cancel immediately

	time.Sleep(50 * time.Millisecond)

	ctrl.mu.Lock()
	calls := ctrl.powerOffCalls
	ctrl.mu.Unlock()
	if calls > 0 {
		t.Error("expected no PowerOff after context cancellation")
	}
}

func TestStopIdleWatcher(t *testing.T) {
	ctrl := newFakeController()
	gw, err := NewGatewayWithPollInterval(
		"http://localhost:99999", 50*time.Millisecond, 2*time.Second,
		10*time.Millisecond, ctrl, slog.Default(),
	)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.StartIdleWatcher(context.Background())
	time.Sleep(20 * time.Millisecond)
	gw.StopIdleWatcher()

	ctrl.mu.Lock()
	calls := ctrl.powerOffCalls
	ctrl.mu.Unlock()

	// Force idle conditions and wait; no new PowerOff calls should happen.
	gw.activeMu.Lock()
	gw.lastActivity = time.Now().Add(-100 * time.Millisecond)
	gw.activeMu.Unlock()
	time.Sleep(100 * time.Millisecond)

	ctrl.mu.Lock()
	callsAfter := ctrl.powerOffCalls
	ctrl.mu.Unlock()
	if callsAfter != calls {
		t.Errorf("expected no additional PowerOff calls after StopIdleWatcher, got %d before and %d after", calls, callsAfter)
	}
}

func TestRetryAfterHeader(t *testing.T) {
	ctrl := newFakeController()
	ctrl.ensureReadyErr = state.ErrBackendError

	requestTimeout := 2 * time.Second
	gw, err := NewGateway("http://localhost:99999", 45*time.Second, requestTimeout, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}

	retryAfter := rec.Header().Get("Retry-After")
	expected := "2" // requestTimeout in seconds per RFC 7231
	if retryAfter != expected {
		t.Errorf("expected Retry-After=%q, got %q", expected, retryAfter)
	}
}

func TestNewGateway_InvalidURL(t *testing.T) {
	ctrl := newFakeController()
	_, err := NewGateway("://invalid-url-with-no-scheme", 0, 2*time.Second, ctrl, slog.Default())
	if err == nil {
		t.Fatal("expected error for invalid backend URL, got nil")
	}
}

func TestReverseProxy_SSEStreaming(t *testing.T) {
	chunks := []string{"data: chunk1\n\n", "data: chunk2\n\n", "data: chunk3\n\n"}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("backend ResponseWriter does not support Flush")
		}
		for _, c := range chunks {
			w.Write([]byte(c))
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	gw.Handler().ServeHTTP(rec, req)

	contentType := rec.Header().Get("Content-Type")
	if contentType != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %q", contentType)
	}
	body := rec.Body.String()
	for _, c := range chunks {
		if !strings.Contains(body, c) {
			t.Errorf("expected body to contain chunk %q, got %q", c, body)
		}
	}
}

func TestResponseTracker_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	tracker := &responseTracker{w: rec}

	// Verify Unwrap returns the underlying writer.
	unwrapped := tracker.Unwrap()
	if unwrapped != rec {
		t.Error("Unwrap should return the original ResponseWriter")
	}

	tracker.WriteHeader(http.StatusOK)
	if !tracker.headerWritten {
		t.Error("headerWritten should be true after WriteHeader")
	}
}

func TestActiveCounter_DecrementsAfterRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	gw.activeMu.Lock()
	active := gw.active
	gw.activeMu.Unlock()

	if active != 0 {
		t.Errorf("expected active=0 after request completes, got %d", active)
	}
}

func TestModelsHandler_StartingWithCache(t *testing.T) {
	ctrl := newFakeController()
	ctrl.setState(state.Starting, nil)

	gw, err := NewGateway("http://localhost:99999", 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	cachedData := []byte(`{"data":[{"id":"cached-model","object":"model"}]}`)
	gw.cachedModels.Store(&modelCache{body: cachedData, contentType: "application/json"})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with cached data when Starting, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cached-model") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
	if rec.Header().Get("X-DockMind-Cached") != "true" {
		t.Error("expected X-DockMind-Cached header when serving from cache")
	}
}

func TestReverseProxy_MidStreamClose(t *testing.T) {
	// Backend writes headers and a partial chunk, then closes the connection.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read request headers (best-effort).
		buf := make([]byte, 1024)
		conn.Read(buf)
		// Write partial HTTP response with headers and one chunk, then close.
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\ndata: partial"))
	}()

	ctrl := newFakeController()
	gw, err := NewGateway("http://"+listener.Addr().String(), 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	// Headers were sent, so responseTracker.headerWritten should be true and
	// no JSON error should be appended.
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (headers sent), got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "proxy_error") {
		t.Errorf("expected no JSON error appended after headers sent, got %q", body)
	}
	if !strings.Contains(body, "data: partial") {
		t.Errorf("expected partial chunk in body, got %q", body)
	}
}

func TestModelsHandler_BackendError500(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	// Pre-populate cache so we can verify it is not overwritten.
	oldCache := []byte(`{"data":[{"id":"old-model","object":"model"}]}`)
	gw.cachedModels.Store(&modelCache{body: oldCache, contentType: "application/json"})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for backend 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "proxy_error") {
		t.Errorf("expected proxy_error in body, got %s", rec.Body.String())
	}
	cached := gw.cachedModels.Load()
	if cached == nil || !strings.Contains(string(cached.body), "old-model") {
		t.Error("cache should not be updated after backend 500")
	}
}

func TestModelsHandler_BackendDropMidResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		conn.Read(buf)
		// Send 200 OK headers with Content-Length larger than the partial body,
		// then close before sending the declared number of bytes.
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"data\":[{\"id\":\"partial"))
	}()

	ctrl := newFakeController()
	gw, err := NewGateway("http://"+listener.Addr().String(), 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	oldCache := []byte(`{"data":[{"id":"old-model","object":"model"}]}`)
	gw.cachedModels.Store(&modelCache{body: oldCache, contentType: "application/json"})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for dropped backend, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "proxy_error") {
		t.Errorf("expected proxy_error in body, got %s", rec.Body.String())
	}
	cached := gw.cachedModels.Load()
	if cached == nil || !strings.Contains(string(cached.body), "old-model") {
		t.Error("cache should not be updated after truncated backend response")
	}
}

func TestModelsHandler_ConcurrentReadsAndRefresh(t *testing.T) {
	modelResponse := `{"data":[{"id":"model-1","object":"model"}]}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(modelResponse))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			rec := httptest.NewRecorder()
			gw.ModelsHandler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()

	cached := gw.cachedModels.Load()
	if cached == nil || !strings.Contains(string(cached.body), "model-1") {
		t.Error("cache should be populated after concurrent refreshes")
	}
}

func TestIdleShutdown_ActiveRequestPreventsShutdown(t *testing.T) {
	block := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.Write([]byte(`{}`))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGatewayWithPollInterval(
		backend.URL, 50*time.Millisecond, 2*time.Second,
		10*time.Millisecond, ctrl, slog.Default(),
	)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.StartIdleWatcher(context.Background())
	defer gw.StopIdleWatcher()

	// Start a request that will block in the backend.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		gw.Handler().ServeHTTP(rec, req)
		close(done)
	}()

	// Wait for the request to be marked active.
	for {
		gw.activeMu.Lock()
		active := gw.active
		gw.activeMu.Unlock()
		if active > 0 {
			break
		}
		time.Sleep(1 * time.Millisecond)
	}

	// Let idle timeout elapse while request is active.
	time.Sleep(150 * time.Millisecond)

	ctrl.mu.Lock()
	calls := ctrl.powerOffCalls
	ctrl.mu.Unlock()
	if calls > 0 {
		t.Error("expected PowerOff NOT called while request is active")
	}

	close(block)
	<-done
}

func TestIdleShutdown_ManualPowerOnInitializesTimer(t *testing.T) {
	ctrl := newFakeController()
	gw, err := NewGatewayWithPollInterval(
		"http://localhost:99999", 50*time.Millisecond, 2*time.Second,
		10*time.Millisecond, ctrl, slog.Default(),
	)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.StartIdleWatcher(context.Background())
	defer gw.StopIdleWatcher()

	// Simulate manual power-on: state transitions to Ready without a gateway request.
	gw.activeMu.Lock()
	gw.lastActivity = time.Time{} // zero value
	gw.activeMu.Unlock()

	ctrl.setState(state.Ready, nil)

	// Wait for watcher to tick and initialize lastActivity.
	time.Sleep(30 * time.Millisecond)

	gw.activeMu.Lock()
	la := gw.lastActivity
	gw.activeMu.Unlock()
	if la.IsZero() {
		t.Error("expected lastActivity initialized after transition to Ready")
	}

	// Now let idle timeout elapse and verify shutdown.
	gw.activeMu.Lock()
	gw.lastActivity = time.Now().Add(-100 * time.Millisecond)
	gw.activeMu.Unlock()

	time.Sleep(150 * time.Millisecond)

	ctrl.mu.Lock()
	calls := ctrl.powerOffCalls
	ctrl.mu.Unlock()
	if calls == 0 {
		t.Error("expected PowerOff called after idle timeout initialized by manual Ready")
	}
}

func TestModelsHandler_DoesNotResetIdleTimer(t *testing.T) {
	modelResponse := `{"data":[{"id":"model-1","object":"model"}]}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(modelResponse))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGatewayWithPollInterval(
		backend.URL, 50*time.Millisecond, 2*time.Second,
		10*time.Millisecond, ctrl, slog.Default(),
	)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.StartIdleWatcher(context.Background())
	defer gw.StopIdleWatcher()

	// Set lastActivity far in the past.
	gw.activeMu.Lock()
	gw.lastActivity = time.Now().Add(-100 * time.Millisecond)
	gw.activeMu.Unlock()

	// Poll models repeatedly while Ready.
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()
		gw.ModelsHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Wait for idle timeout to elapse.
	time.Sleep(150 * time.Millisecond)

	ctrl.mu.Lock()
	calls := ctrl.powerOffCalls
	ctrl.mu.Unlock()
	if calls == 0 {
		t.Error("expected PowerOff called after idle timeout despite models polling")
	}
}

func TestInitModelsCache_DisabledLogsWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctrl := newFakeController()
	gw, err := NewGateway("http://localhost:1234", 0, 2*time.Second, ctrl, logger)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.InitModelsCache("")

	if gw.cacheStore != nil {
		t.Error("expected cacheStore nil when dir is empty")
	}
	if gw.cachedModels.Load() != nil {
		t.Error("expected cachedModels nil when dir is empty")
	}
	logOut := buf.String()
	if !strings.Contains(logOut, "modelsCacheDir") {
		t.Errorf("expected warning about modelsCacheDir, got: %s", logOut)
	}
}

func TestInitModelsCache_LoadsExistingFile(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`{"data":[{"id":"model-1","object":"model"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "models.json"), content, 0o644); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	ctrl := newFakeController()
	gw, err := NewGateway("http://localhost:1234", 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.InitModelsCache(dir)

	cached := gw.cachedModels.Load()
	if cached == nil {
		t.Fatal("expected cached models loaded from disk")
	}
	if !bytes.Equal(cached.body, content) {
		t.Errorf("expected body %q, got %q", content, cached.body)
	}
	if cached.contentType != "application/json" {
		t.Errorf("expected contentType application/json, got %q", cached.contentType)
	}
}

func TestInitModelsCache_MissingFileIsNormal(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	ctrl := newFakeController()
	gw, err := NewGateway("http://localhost:1234", 0, 2*time.Second, ctrl, logger)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.InitModelsCache(dir)

	if gw.cachedModels.Load() != nil {
		t.Error("expected no cache loaded when file is missing")
	}
	if buf.Len() > 0 {
		t.Errorf("expected no warning for missing file, got: %s", buf.String())
	}
}

func TestInitModelsCache_EmptyFileIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte{}, 0o644); err != nil {
		t.Fatalf("write empty cache file: %v", err)
	}

	ctrl := newFakeController()
	gw, err := NewGateway("http://localhost:1234", 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.InitModelsCache(dir)

	if gw.cachedModels.Load() != nil {
		t.Error("expected empty file to be ignored")
	}
}

func TestInitModelsCache_ReadErrorLogsWarning(t *testing.T) {
	// Create a path where a parent component is a regular file, causing a read error.
	tmp := t.TempDir()
	badDir := filepath.Join(tmp, "notadir")
	if err := os.WriteFile(badDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("create file at cache dir path: %v", err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	ctrl := newFakeController()
	gw, err := NewGateway("http://localhost:1234", 0, 2*time.Second, ctrl, logger)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.InitModelsCache(badDir)

	if gw.cachedModels.Load() != nil {
		t.Error("expected no cache loaded on read error")
	}
	logOut := buf.String()
	if !strings.Contains(logOut, "failed to load cached models") {
		t.Errorf("expected warning about failed load, got: %s", logOut)
	}
}

func TestModelsHandler_PersistsCacheOnSuccess(t *testing.T) {
	dir := t.TempDir()
	modelResponse := `{"data":[{"id":"model-1","object":"model"}]}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(modelResponse))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw.InitModelsCache(dir)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model-1") {
		t.Errorf("expected response body to contain model-1, got: %s", rec.Body.String())
	}

	cached := gw.cachedModels.Load()
	if cached == nil || !strings.Contains(string(cached.body), "model-1") {
		t.Error("expected in-memory cache updated")
	}

	written, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("expected cache file written: %v", err)
	}
	if string(written) != modelResponse {
		t.Errorf("expected file content %q, got %q", modelResponse, written)
	}
}

func TestModelsHandler_DoesNotPersistCacheOnBackend500(t *testing.T) {
	dir := t.TempDir()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw.InitModelsCache(dir)

	oldCache := []byte(`{"data":[{"id":"old-model","object":"model"}]}`)
	gw.cachedModels.Store(&modelCache{body: oldCache, contentType: "application/json"})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}

	cached := gw.cachedModels.Load()
	if cached == nil || !strings.Contains(string(cached.body), "old-model") {
		t.Error("expected in-memory cache unchanged")
	}

	if _, err := os.ReadFile(filepath.Join(dir, "models.json")); !os.IsNotExist(err) {
		t.Errorf("expected no cache file written for backend 500, got err: %v", err)
	}
}

func TestModelsHandler_NoPersistenceWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	modelResponse := `{"data":[{"id":"model-1","object":"model"}]}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(modelResponse))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw.InitModelsCache("")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if _, err := os.ReadFile(filepath.Join(dir, "models.json")); !os.IsNotExist(err) {
		t.Errorf("expected no cache file when persistence disabled, got err: %v", err)
	}
}

func TestModelsHandler_SaveFailureLogsError(t *testing.T) {
	// Create a path where the directory cannot be created because a parent is a file.
	tmp := t.TempDir()
	badDir := filepath.Join(tmp, "notadir")
	if err := os.WriteFile(badDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("create file at cache dir path: %v", err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	modelResponse := `{"data":[{"id":"model-1","object":"model"}]}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(modelResponse))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, logger)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw.InitModelsCache(badDir)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite save failure, got %d: %s", rec.Code, rec.Body.String())
	}

	cached := gw.cachedModels.Load()
	if cached == nil || !strings.Contains(string(cached.body), "model-1") {
		t.Error("expected in-memory cache updated even when save fails")
	}

	logOut := buf.String()
	if !strings.Contains(logOut, "failed to persist cached models") {
		t.Errorf("expected error log about save failure, got: %s", logOut)
	}
}

func TestModelsHandler_ServesDiskCacheWhenOff(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`{"data":[{"id":"disk-model","object":"model"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "models.json"), content, 0o644); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	ctrl := newFakeController()
	ctrl.setState(state.Off, nil)
	gw, err := NewGateway("http://localhost:99999", 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw.InitModelsCache(dir)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from disk cache, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "disk-model") {
		t.Errorf("expected disk cache body, got: %s", rec.Body.String())
	}
	if rec.Header().Get("X-DockMind-Cached") != "true" {
		t.Error("expected X-DockMind-Cached header")
	}
}

func TestModelsHandler_ServesDiskCacheWhenStarting(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`{"data":[{"id":"disk-model","object":"model"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "models.json"), content, 0o644); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	ctrl := newFakeController()
	ctrl.setState(state.Starting, nil)
	gw, err := NewGateway("http://localhost:99999", 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw.InitModelsCache(dir)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from disk cache, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "disk-model") {
		t.Errorf("expected disk cache body, got: %s", rec.Body.String())
	}
	if rec.Header().Get("X-DockMind-Cached") != "true" {
		t.Error("expected X-DockMind-Cached header")
	}
}

func TestModelsHandler_CreatesCacheDirOnSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cache")
	modelResponse := `{"data":[{"id":"model-1","object":"model"}]}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(modelResponse))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw.InitModelsCache(dir)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	written, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("expected cache file created in new directory: %v", err)
	}
	if string(written) != modelResponse {
		t.Errorf("expected file content %q, got %q", modelResponse, written)
	}
}

func TestModelCacheStore_SaveSkipsIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	store := newModelCacheStore(dir)
	cache := &modelCache{body: []byte(`{"data":[]}`), contentType: "application/json"}

	if err := store.save(cache); err != nil {
		t.Fatalf("first save failed: %v", err)
	}
	firstHash := store.lastWrittenHash

	if err := store.save(cache); err != nil {
		t.Fatalf("second save failed: %v", err)
	}
	if store.lastWrittenHash != firstHash {
		t.Error("expected lastWrittenHash unchanged after redundant save")
	}

	written, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	if string(written) != string(cache.body) {
		t.Errorf("expected file content unchanged, got %q", written)
	}
}

func TestModelCacheStore_SaveWritesDifferentContent(t *testing.T) {
	dir := t.TempDir()
	store := newModelCacheStore(dir)
	cacheA := &modelCache{body: []byte(`{"data":[{"id":"a"}]}`), contentType: "application/json"}
	cacheB := &modelCache{body: []byte(`{"data":[{"id":"b"}]}`), contentType: "application/json"}

	if err := store.save(cacheA); err != nil {
		t.Fatalf("save cacheA failed: %v", err)
	}
	if err := store.save(cacheB); err != nil {
		t.Fatalf("save cacheB failed: %v", err)
	}

	if store.lastWrittenHash != fnv64(cacheB.body) {
		t.Error("expected lastWrittenHash updated to cacheB hash")
	}

	written, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	if string(written) != string(cacheB.body) {
		t.Errorf("expected file content %q, got %q", cacheB.body, written)
	}
}

func TestModelCacheStore_LoadThenSaveSkipsIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`{"data":[{"id":"a"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "models.json"), content, 0o644); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	store := newModelCacheStore(dir)
	cache, err := store.load()
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if cache == nil {
		t.Fatal("expected cache loaded")
	}
	if store.lastWrittenHash != fnv64(content) {
		t.Error("expected lastWrittenHash set by load")
	}

	if err := store.save(cache); err != nil {
		t.Fatalf("save after load failed: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("stat cache file: %v", err)
	}
	modTime := info.ModTime()

	// Save again with identical content; file should not be rewritten.
	if err := store.save(cache); err != nil {
		t.Fatalf("second save failed: %v", err)
	}
	info, err = os.Stat(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("stat cache file after second save: %v", err)
	}
	if !info.ModTime().Equal(modTime) {
		t.Error("expected file not rewritten after load-then-save with identical content")
	}
}

func TestModelCacheStore_LoadMissingThenSaveWrites(t *testing.T) {
	dir := t.TempDir()
	store := newModelCacheStore(dir)
	cache, err := store.load()
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if cache != nil {
		t.Error("expected no cache for missing file")
	}
	if store.lastWrittenHash != 0 {
		t.Error("expected lastWrittenHash zero after missing file load")
	}

	cache = &modelCache{body: []byte(`{"data":[]}`), contentType: "application/json"}
	if err := store.save(cache); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if store.lastWrittenHash != fnv64(cache.body) {
		t.Error("expected lastWrittenHash updated after save")
	}

	written, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	if string(written) != string(cache.body) {
		t.Errorf("expected file content %q, got %q", cache.body, written)
	}
}

func TestModelsHandler_ConcurrentSavesNoRace(t *testing.T) {
	dir := t.TempDir()
	modelResponse := `{"data":[{"id":"model-1","object":"model"}]}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(modelResponse))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	gw, err := NewGateway(backend.URL, 0, 2*time.Second, ctrl, slog.Default())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw.InitModelsCache(dir)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			rec := httptest.NewRecorder()
			gw.ModelsHandler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()

	written, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("expected cache file written: %v", err)
	}
	if string(written) != modelResponse {
		t.Errorf("expected file content %q, got %q", modelResponse, written)
	}
}

func TestFullGatewayFlow(t *testing.T) {
	var requestCount int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"model-1","object":"model"}]}`))
			return
		}
		w.Write([]byte(`{"result":"ok"}`))
	}))
	defer backend.Close()

	ctrl := newFakeController()
	ctrl.setState(state.Off, nil)
	ctrl.autoStart = true

	gw, err := NewGatewayWithPollInterval(
		backend.URL, 50*time.Millisecond, 2*time.Second,
		10*time.Millisecond, ctrl, slog.Default(),
	)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	gw.StartIdleWatcher(context.Background())
	defer gw.StopIdleWatcher()

	// Inference request auto-starts backend and proxies response.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"result":"ok"`) {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}

	// Models request populates cache.
	mreq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	mrec := httptest.NewRecorder()
	gw.ModelsHandler().ServeHTTP(mrec, mreq)
	if mrec.Code != http.StatusOK {
		t.Fatalf("expected 200 for models, got %d: %s", mrec.Code, mrec.Body.String())
	}
	cached := gw.cachedModels.Load()
	if cached == nil || !strings.Contains(string(cached.body), "model-1") {
		t.Error("expected cache populated")
	}

	// Wait for idle timeout to trigger auto-shutdown.
	gw.activeMu.Lock()
	gw.lastActivity = time.Now().Add(-100 * time.Millisecond)
	gw.activeMu.Unlock()
	time.Sleep(200 * time.Millisecond)

	ctrl.mu.Lock()
	calls := ctrl.powerOffCalls
	ctrl.mu.Unlock()
	if calls == 0 {
		t.Error("expected PowerOff called after idle timeout")
	}
}
