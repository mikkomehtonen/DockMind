package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		want       bool
		wantModels []string
		wantLen    int
		wantErr    bool
	}{
		{
			name:       "healthy 200 empty running",
			statusCode: http.StatusOK,
			body:       `{"running":[]}`,
			want:       true,
			wantLen:    0,
		},
		{
			name:       "healthy 200 starting model",
			statusCode: http.StatusOK,
			body:       `{"running":[{"model":"qwen3.5-9b","state":"starting"}]}`,
			want:       true,
			wantModels: []string{"qwen3.5-9b"},
		},
		{
			name:       "healthy 200 ready model",
			statusCode: http.StatusOK,
			body:       `{"running":[{"model":"qwen3.5-9b","state":"ready"}]}`,
			want:       true,
			wantModels: []string{"qwen3.5-9b"},
		},
		{
			name:       "healthy 200 two models",
			statusCode: http.StatusOK,
			body:       `{"running":[{"model":"model-a","state":"ready"},{"model":"model-b","state":"starting"}]}`,
			want:       true,
			wantModels: []string{"model-a", "model-b"},
		},
		{
			name:       "healthy 200 empty model filtered",
			statusCode: http.StatusOK,
			body:       `{"running":[{"model":"","state":"ready"}]}`,
			want:       true,
			wantLen:    0,
		},
		{
			name:       "unhealthy 500",
			statusCode: http.StatusInternalServerError,
			body:       `{"error":"boom"}`,
			want:       false,
			wantLen:    0,
		},
		{
			name:       "not found 404",
			statusCode: http.StatusNotFound,
			body:       `not found`,
			want:       false,
			wantLen:    0,
		},
		{
			name:       "healthy 200 malformed json",
			statusCode: http.StatusOK,
			body:       `not json`,
			want:       false,
			wantLen:    0,
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

			client := New(server.URL)
			got, models, err := client.Check(context.Background())
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
				t.Fatalf("expected healthy %v, got %v", tc.want, got)
			}
			if tc.wantModels != nil {
				if !reflect.DeepEqual(models, tc.wantModels) {
					t.Fatalf("expected models %v, got %v", tc.wantModels, models)
				}
			} else if len(models) != tc.wantLen {
				t.Fatalf("expected models len %d, got %v", tc.wantLen, models)
			}
		})
	}
}

func TestCheckUnreachable(t *testing.T) {
	client := New("http://127.0.0.1:1/health")
	got, _, err := client.Check(context.Background())
	if err == nil {
		t.Fatalf("expected error for unreachable server, got nil")
	}
	if got {
		t.Fatalf("expected false for unreachable server")
	}
}

func TestUnloadClient(t *testing.T) {
	t.Run("200 OK returns nil and uses GET /unload", func(t *testing.T) {
		var gotMethod, gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		}))
		defer server.Close()

		client := NewUnloadClient(server.URL)
		if err := client.Unload(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotMethod != http.MethodGet {
			t.Fatalf("expected GET, got %q", gotMethod)
		}
		if gotPath != "/unload" {
			t.Fatalf("expected path /unload, got %q", gotPath)
		}
	})

	t.Run("500 returns error containing status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		client := NewUnloadClient(server.URL)
		err := client.Unload(context.Background())
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "status 500") {
			t.Fatalf("expected error to contain 'status 500', got %q", err.Error())
		}
	})

	t.Run("unreachable server returns error", func(t *testing.T) {
		client := NewUnloadClient("http://127.0.0.1:1")
		if err := client.Unload(context.Background()); err == nil {
			t.Fatal("expected error for unreachable server, got nil")
		}
	})

	t.Run("trailing slash on backendURL yields /unload path", func(t *testing.T) {
		var gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := NewUnloadClient(server.URL + "/")
		if err := client.Unload(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotPath != "/unload" {
			t.Fatalf("expected path /unload, got %q", gotPath)
		}
	})
}
