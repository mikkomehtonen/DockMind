package shelly

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetPower(t *testing.T) {
	cases := []struct {
		name       string
		on         bool
		channel    int
		statusCode int
		wantErr    bool
		wantPath   string
	}{
		{
			name:       "set power on",
			on:         true,
			channel:    0,
			statusCode: http.StatusOK,
			wantPath:   "/rpc/Switch.Set?id=0&on=true",
		},
		{
			name:       "set power off",
			on:         false,
			channel:    0,
			statusCode: http.StatusOK,
			wantPath:   "/rpc/Switch.Set?id=0&on=false",
		},
		{
			name:       "channel 1",
			on:         true,
			channel:    1,
			statusCode: http.StatusOK,
			wantPath:   "/rpc/Switch.Set?id=1&on=true",
		},
		{
			name:       "non-200 response",
			on:         true,
			channel:    0,
			statusCode: http.StatusInternalServerError,
			wantErr:    true,
			wantPath:   "/rpc/Switch.Set?id=0&on=true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.RequestURI()
				w.WriteHeader(tc.statusCode)
			}))
			defer server.Close()

			client := New(server.Listener.Addr().String(), tc.channel)
			err := client.SetPower(context.Background(), tc.on)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !strings.Contains(gotPath, tc.wantPath) {
				t.Fatalf("expected path %q, got %q", tc.wantPath, gotPath)
			}
		})
	}
}

func TestSetPowerUnreachable(t *testing.T) {
	client := New("127.0.0.1:1", 0)
	err := client.SetPower(context.Background(), true)
	if err == nil {
		t.Fatalf("expected error for unreachable server, got nil")
	}
}

func TestIsOn(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		statusCode int
		want       bool
		wantErr    bool
	}{
		{
			name:       "output true",
			body:       `{"output": true}`,
			statusCode: http.StatusOK,
			want:       true,
		},
		{
			name:       "output false",
			body:       `{"output": false}`,
			statusCode: http.StatusOK,
			want:       false,
		},
		{
			name:       "non-200 response",
			body:       "",
			statusCode: http.StatusInternalServerError,
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client := New(server.Listener.Addr().String(), 0)
			got, err := client.IsOn(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}
