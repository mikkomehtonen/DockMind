package api

import (
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/dockmind/dockmind/internal/state"
)

//go:embed openapi.json
var openapiSpec []byte

//go:embed docs.html
var docsHTML []byte

//go:embed index.html
var indexHTML []byte

type StateMachine interface {
	Status() state.StatusResponse
	PowerOn() state.PowerResult
	PowerOff() state.PowerResult
	Restart() state.PowerResult
}

type Server struct {
	machine StateMachine
	logger  *slog.Logger
}

func NewServer(machine StateMachine, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		machine: machine,
		logger:  logger,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /power/on", s.handlePowerOn)
	mux.HandleFunc("POST /power/off", s.handlePowerOff)
	mux.HandleFunc("POST /restart", s.handleRestart)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /docs", s.handleDocs)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	return mux
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := s.machine.Status()
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

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(indexHTML)
}
