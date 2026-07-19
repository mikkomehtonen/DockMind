package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html"
	"log/slog"
	"net/http"
	"os"

	"github.com/dockmind/dockmind/internal/state"
)

//go:embed openapi.json
var openapiSpec []byte

//go:embed docs.html
var docsHTML []byte

//go:embed index.html
var indexHTML []byte

//go:embed favicon.svg
var faviconSVG []byte

type StateMachine interface {
	Status() state.StatusResponse
	PowerOn() state.PowerResult
	PowerOff() state.PowerResult
	Restart() state.PowerResult
	StartAuxContainer(name string) state.AuxResult
	StopAuxContainer(name string) state.AuxResult
}

// IdleReporter reports the remaining time before an idle auto-shutdown.
// Implementations return 0 when no shutdown is pending.
type IdleReporter interface {
	IdleRemaining() float64
}

type Server struct {
	machine           StateMachine
	logger            *slog.Logger
	indexHTMLRendered []byte
	gatewayHandler    http.Handler // nil = gateway disabled
	modelsHandler     http.Handler // nil = gateway disabled
	idleReporter      IdleReporter // nil = no idle reporter wired
}

func NewServer(machine StateMachine, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		machine: machine,
		logger:  logger,
	}
	s.indexHTMLRendered = indexHTML
	if logoURL := os.Getenv("LOGO_LINK_URL"); logoURL != "" {
		const logoImg = `<img src="/favicon.svg" alt="DockMind" class="app__logo" width="24" height="24">`
		escaped := html.EscapeString(logoURL)
		s.indexHTMLRendered = bytes.Replace(
			indexHTML,
			[]byte(logoImg),
			[]byte(`<a href="`+escaped+`" class="app__logo-link">`+logoImg+`</a>`),
			1,
		)
	}
	return s
}

// SetGatewayHandlers registers the OpenAI-compatible gateway handlers.
func (s *Server) SetGatewayHandlers(inference, models http.Handler) {
	s.gatewayHandler = inference
	s.modelsHandler = models
}

// SetIdleReporter wires the source for the idle auto-shutdown countdown.
func (s *Server) SetIdleReporter(r IdleReporter) {
	s.idleReporter = r
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /power/on", s.handlePowerOn)
	mux.HandleFunc("POST /power/off", s.handlePowerOff)
	mux.HandleFunc("POST /restart", s.handleRestart)
	mux.HandleFunc("POST /containers/{name}/start", s.handleStartAuxContainer)
	mux.HandleFunc("POST /containers/{name}/stop", s.handleStopAuxContainer)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /docs", s.handleDocs)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /favicon.svg", s.handleFavicon)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	if s.modelsHandler != nil {
		mux.Handle("GET /v1/models", s.modelsHandler)
	}
	if s.gatewayHandler != nil {
		mux.Handle("/v1/{rest...}", s.gatewayHandler)
	}
	return mux
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := s.machine.Status()
	if s.idleReporter != nil {
		status.IdleRemaining = s.idleReporter.IdleRemaining()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(status); err != nil {
		s.logger.Error("failed to encode status", "error", err)
	}
}

func (s *Server) handlePowerOn(w http.ResponseWriter, r *http.Request) {
	s.handlePowerResult(w, s.machine.PowerOn())
}

func (s *Server) handlePowerOff(w http.ResponseWriter, r *http.Request) {
	s.handlePowerResult(w, s.machine.PowerOff())
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	s.handlePowerResult(w, s.machine.Restart())
}

func (s *Server) handlePowerResult(w http.ResponseWriter, result state.PowerResult) {
	switch result {
	case state.ResultAccepted:
		w.WriteHeader(http.StatusAccepted)
	case state.ResultAlreadyInState:
		w.WriteHeader(http.StatusOK)
	case state.ResultConflict:
		w.WriteHeader(http.StatusConflict)
	case state.ResultCooldown:
		w.WriteHeader(http.StatusTooManyRequests)
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (s *Server) handleStartAuxContainer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.handleAuxResult(w, s.machine.StartAuxContainer(name))
}

func (s *Server) handleStopAuxContainer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.handleAuxResult(w, s.machine.StopAuxContainer(name))
}

func (s *Server) handleAuxResult(w http.ResponseWriter, result state.AuxResult) {
	switch result {
	case state.AuxResultOK:
		w.WriteHeader(http.StatusOK)
	case state.AuxResultNotFound:
		w.WriteHeader(http.StatusNotFound)
	case state.AuxResultConflict:
		w.WriteHeader(http.StatusConflict)
	case state.AuxResultError:
		w.WriteHeader(http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(docsHTML)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(openapiSpec)
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.WriteHeader(http.StatusOK)
	w.Write(faviconSVG)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(s.indexHTMLRendered)
}
