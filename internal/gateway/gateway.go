package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dockmind/dockmind/internal/state"
)

// StateController is the subset of the real state machine needed by the gateway.
type StateController interface {
	State() state.State
	PowerOff() state.PowerResult
	EnsureReady(ctx context.Context) error
}

// modelCache holds a cached copy of the /v1/models response.
type modelCache struct {
	body        []byte
	contentType string
}

// modelCacheStore handles disk persistence for the cached model list.
type modelCacheStore struct {
	path            string
	mu              sync.Mutex
	lastWrittenHash uint64 // FNV-1a hash of the last content written to disk
}

func newModelCacheStore(dir string) *modelCacheStore {
	return &modelCacheStore{
		path: filepath.Join(dir, "models.json"),
	}
}

func fnv64(data []byte) uint64 {
	h := fnv.New64a()
	h.Write(data)
	return h.Sum64()
}

func (s *modelCacheStore) load() (*modelCache, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	s.lastWrittenHash = fnv64(data)
	return &modelCache{
		body:        data,
		contentType: "application/json",
	}, nil
}

func (s *modelCacheStore) save(cache *modelCache) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := fnv64(cache.body)
	if h == s.lastWrittenHash {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, cache.body, 0o644); err != nil {
		return err
	}
	s.lastWrittenHash = h
	return nil
}

// Gateway handles OpenAI-compatible API reverse proxying with auto-start and idle shutdown.
type Gateway struct {
	backendURL     *url.URL
	idleTimeout    time.Duration // 0 = disabled
	requestTimeout time.Duration
	pollInterval   time.Duration
	machine        StateController
	logger         *slog.Logger
	client         *http.Client

	cachedModels atomic.Pointer[modelCache]
	cacheStore   *modelCacheStore

	// Active request tracking for idle shutdown.
	activeMu        sync.Mutex
	active          int // number of active inference requests
	lastActivity    time.Time
	pendingShutdown bool

	// Idle watcher lifecycle.
	idleCtx    context.Context
	idleCancel context.CancelFunc
}

// NewGateway creates a new Gateway. Returns error if backendURL is invalid.
func NewGateway(backendURL string, idleTimeout, requestTimeout time.Duration, machine StateController, logger *slog.Logger) (*Gateway, error) {
	u, err := url.Parse(backendURL)
	if err != nil {
		return nil, fmt.Errorf("parse backend URL: %w", err)
	}

	gw := &Gateway{
		backendURL:     u,
		idleTimeout:    idleTimeout,
		requestTimeout: requestTimeout,
		pollInterval:   time.Second,
		machine:        machine,
		logger:         logger,
		lastActivity:   time.Now(),
		client: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: requestTimeout,
			},
		},
	}

	return gw, nil
}

// NewGatewayWithPollInterval creates a Gateway with an explicit poll interval.
func NewGatewayWithPollInterval(backendURL string, idleTimeout, requestTimeout, pollInterval time.Duration, machine StateController, logger *slog.Logger) (*Gateway, error) {
	gw, err := NewGateway(backendURL, idleTimeout, requestTimeout, machine, logger)
	if err != nil {
		return nil, err
	}
	gw.pollInterval = pollInterval
	return gw, nil
}

// InitModelsCache initializes disk persistence for the cached model list.
func (g *Gateway) InitModelsCache(dir string) {
	if dir == "" {
		g.logger.Warn("gateway modelsCacheDir is not configured; cached model list will not persist across restarts")
		return
	}
	g.cacheStore = newModelCacheStore(dir)
	cache, err := g.cacheStore.load()
	if err != nil {
		g.logger.Warn("failed to load cached models from disk", "error", err, "dir", dir)
		return
	}
	if cache != nil {
		g.cachedModels.Store(cache)
		g.logger.Info("loaded cached models from disk")
	}
}

// StartIdleWatcher starts the background idle shutdown goroutine.
// If idleTimeout is 0, the watcher exits immediately (no-op).
func (g *Gateway) StartIdleWatcher(ctx context.Context) {
	if g.idleTimeout <= 0 {
		return
	}
	g.idleCtx, g.idleCancel = context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(g.pollInterval)
		defer ticker.Stop()

		var prevReady bool
		for {
			select {
			case <-g.idleCtx.Done():
				return
			case <-ticker.C:
				g.tick(prevReady)
				prevReady = g.machine.State() == state.Ready
			}
		}
	}()
}

// StopIdleWatcher stops the idle watcher goroutine.
func (g *Gateway) StopIdleWatcher() {
	if g.idleCancel != nil {
		g.idleCancel()
	}
}

func (g *Gateway) tick(prevReady bool) {
	current := g.machine.State()

	// If state just transitioned to Ready, initialize lastActivity.
	if current == state.Ready && !prevReady {
		g.activeMu.Lock()
		g.lastActivity = time.Now()
		g.pendingShutdown = false
		g.activeMu.Unlock()
		return
	}

	// Only act when in Ready state with no active requests.
	g.activeMu.Lock()
	if current != state.Ready || g.active > 0 {
		g.activeMu.Unlock()
		return
	}

	idle := time.Since(g.lastActivity)
	pending := g.pendingShutdown
	g.activeMu.Unlock()

	if !pending && idle >= g.idleTimeout {
		// Phase 1: reserve shutdown (grace period).
		g.activeMu.Lock()
		if g.active == 0 && !g.pendingShutdown {
			g.pendingShutdown = true
		}
		g.activeMu.Unlock()
		return
	}

	if pending {
		// Phase 2: confirm shutdown after grace period tick.
		// Re-check under the lock to close the race window between Phase 1
		// and the actual PowerOff call.
		g.activeMu.Lock()
		stillIdle := g.active == 0 && g.pendingShutdown && g.machine.State() == state.Ready
		if stillIdle {
			g.pendingShutdown = false
		}
		g.activeMu.Unlock()
		if !stillIdle {
			return
		}

		result := g.machine.PowerOff()
		switch result {
		case state.ResultAccepted:
			g.logger.Info("idle shutdown initiated")
		default:
			g.activeMu.Lock()
			g.pendingShutdown = false
			g.activeMu.Unlock()
		}
	}
}

// Handler returns the inference handler for /v1/{rest...}.
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(g.handleInference)
}

func (g *Gateway) handleInference(w http.ResponseWriter, r *http.Request) {
	// Mark request active.
	g.activeMu.Lock()
	g.active++
	g.lastActivity = time.Now()
	if g.pendingShutdown {
		g.pendingShutdown = false // Request arrived during grace period — cancel shutdown.
	}
	g.activeMu.Unlock()

	defer func() {
		g.activeMu.Lock()
		g.active--
		g.lastActivity = time.Now()
		g.activeMu.Unlock()
	}()

	// Ensure the backend is ready (auto-start if needed).
	ctx, cancel := g.withRequestDeadline(r.Context())
	defer cancel()
	if err := g.machine.EnsureReady(ctx); err != nil {
		g.writeEnsureReadyError(w, r, err)
		return
	}

	g.proxyToBackend(w, r)
}

// ModelsHandler returns the model-list handler for GET /v1/models.
func (g *Gateway) ModelsHandler() http.Handler {
	return http.HandlerFunc(g.handleModels)
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	current := g.machine.State()

	// Error state: never serve cache — backend has a problem.
	if current == state.Error {
		g.writeError(w, http.StatusServiceUnavailable, "backend_error")
		return
	}

	// Non-Ready states: serve from cache if available.
	if current != state.Ready {
		cached := g.cachedModels.Load()
		if cached != nil && len(cached.body) > 0 {
			w.Header().Set("Content-Type", cached.contentType)
			w.Header().Set("X-DockMind-Cached", "true")
			w.WriteHeader(http.StatusOK)
			w.Write(cached.body)
			return
		}

		g.writeError(w, http.StatusServiceUnavailable, "model_cache_unavailable")
		return
	}

	// Ready state: proxy to backend with buffering for caching.
	// Count models requests as active while proxying to avoid racing with idle shutdown.
	g.activeMu.Lock()
	g.active++
	g.pendingShutdown = false
	g.activeMu.Unlock()
	defer func() {
		g.activeMu.Lock()
		g.active--
		g.activeMu.Unlock()
	}()

	bw := &bufferingWriter{
		buf:    bytes.NewBuffer(nil),
		header: make(http.Header),
	}
	g.proxyWithErrorHandler(r, bw)

	if bw.failed {
		g.writeError(w, http.StatusBadGateway, "proxy_error")
		return
	}

	// Successful response — cache and flush to client.
	contentType := bw.header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	cache := &modelCache{
		body:        bw.buf.Bytes(),
		contentType: contentType,
	}
	g.cachedModels.Store(cache)
	if g.cacheStore != nil {
		if err := g.cacheStore.save(cache); err != nil {
			g.logger.Error("failed to persist cached models to disk", "error", err)
		}
	}

	// Now write the cached response to the actual ResponseWriter.
	for key, values := range bw.header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	if bw.statusCode > 0 {
		w.WriteHeader(bw.statusCode)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	w.Write(cache.body)
}

// proxyToBackend proxies the request using httputil.ReverseProxy with streaming.
func (g *Gateway) proxyToBackend(w http.ResponseWriter, r *http.Request) {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(g.backendURL)
			pr.SetXForwarded()
		},
		FlushInterval: -1, // Flush after every write for SSE support.
		Transport:     g.client.Transport,
		ErrorHandler:  g.errorHandler(),
	}

	tracker := &responseTracker{w: w}
	proxy.ServeHTTP(tracker, r)
}

func (g *Gateway) errorHandler() func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, req *http.Request, err error) {
		g.logger.Error("proxy error", "error", err, "path", req.URL.Path)

		// Check if headers were already written to the client — don't corrupt.
		if rt, ok := w.(*responseTracker); ok && rt.headerWritten {
			return // Headers sent; can't write error JSON. Client gets truncated stream.
		}

		g.writeError(w, http.StatusBadGateway, "proxy_error")
	}
}

// proxyWithErrorHandler proxies to the backend for model-list caching (non-streaming).
func (g *Gateway) proxyWithErrorHandler(r *http.Request, bw *bufferingWriter) {
	ctx, cancel := g.withRequestDeadline(r.Context())
	defer cancel()
	req, err := g.newBackendRequest(ctx, r)
	if err != nil {
		bw.failed = true
		return
	}

	resp, err := g.client.Do(req)
	if err != nil {
		bw.failed = true
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		bw.failed = true
		return
	}

	for key, values := range resp.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, v := range values {
			bw.header.Add(key, v)
		}
	}
	bw.statusCode = resp.StatusCode
	bw.buf.Write(body)
}

// isHopByHopHeader reports whether a header should not be forwarded to the client.
func isHopByHopHeader(key string) bool {
	switch key {
	case "Connection", "Keep-Alive", "Transfer-Encoding", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Upgrade":
		return true
	}
	return false
}

func (g *Gateway) newBackendRequest(ctx context.Context, r *http.Request) (*http.Request, error) {
	req := r.Clone(ctx)
	req.URL.Scheme = g.backendURL.Scheme
	req.URL.Host = g.backendURL.Host
	req.Host = req.URL.Host
	req.RequestURI = ""
	return req, nil
}

// withRequestDeadline adds a deadline to the context based on requestTimeout.
func (g *Gateway) withRequestDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if g.requestTimeout > 0 {
		return context.WithDeadline(ctx, time.Now().Add(g.requestTimeout))
	}
	return ctx, func() {}
}

// writeEnsureReadyError maps EnsureReady errors to proper HTTP responses.
func (g *Gateway) writeEnsureReadyError(w http.ResponseWriter, r *http.Request, err error) {
	// Check ErrBackendError BEFORE context.DeadlineExceeded/Canceled.
	if errors.Is(err, state.ErrBackendError) {
		g.logger.Debug("backend in error state", "error", err, "path", r.URL.Path)
		w.Header().Set("Retry-After", g.retryAfterSeconds())
		g.writeError(w, http.StatusServiceUnavailable, "backend_error")
		return
	}

	if errors.Is(err, context.DeadlineExceeded) {
		g.logger.Debug("startup timed out", "path", r.URL.Path)
		w.Header().Set("Retry-After", g.retryAfterSeconds())
		g.writeError(w, http.StatusServiceUnavailable, "startup_timeout")
		return
	}

	// context.Canceled: client disconnected — don't write anything.
	if errors.Is(err, context.Canceled) {
		return
	}

	// Unknown error — treat as bad gateway.
	g.logger.Error("unexpected EnsureReady error", "error", err, "path", r.URL.Path)
	g.writeError(w, http.StatusBadGateway, "proxy_error")
}

func (g *Gateway) retryAfterSeconds() string {
	// Use requestTimeout as the retry hint because it bounds the startup wait.
	// Fallback to a small fixed value when requestTimeout is not configured.
	if g.requestTimeout > 0 {
		return strconv.Itoa(int(g.requestTimeout.Seconds()))
	}
	return "10"
}

func (g *Gateway) writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIError{
		Error: openAIErrorBody{
			Message: code,
			Type:    errorTypeForStatus(status),
			Code:    code,
		},
	})
}

func errorTypeForStatus(status int) string {
	if status == http.StatusBadGateway {
		return "bad_gateway"
	}
	return "service_unavailable"
}

// openAI error types for JSON error responses.
type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

// responseTracker wraps http.ResponseWriter to track whether headers were written.
type responseTracker struct {
	w             http.ResponseWriter
	headerWritten bool
}

func (rt *responseTracker) Header() http.Header {
	return rt.w.Header()
}

func (rt *responseTracker) Write(p []byte) (int, error) {
	if !rt.headerWritten {
		rt.headerWritten = true
	}
	return rt.w.Write(p)
}

func (rt *responseTracker) WriteHeader(statusCode int) {
	if !rt.headerWritten {
		rt.headerWritten = true
	}
	rt.w.WriteHeader(statusCode)
}

// Unwrap returns the underlying ResponseWriter for http.ResponseController.Flush() support.
func (rt *responseTracker) Unwrap() http.ResponseWriter {
	return rt.w
}

// bufferingWriter captures full response without sending to client.
type bufferingWriter struct {
	buf        *bytes.Buffer
	header     http.Header
	statusCode int
	failed     bool
}
