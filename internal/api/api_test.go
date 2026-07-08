package api

import (
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
