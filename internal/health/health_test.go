package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
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
