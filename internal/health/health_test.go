package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheck(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		want       bool
		wantErr    bool
	}{
		{
			name:       "healthy 200",
			statusCode: http.StatusOK,
			want:       true,
		},
		{
			name:       "unhealthy 500",
			statusCode: http.StatusInternalServerError,
			want:       false,
		},
		{
			name:       "not found 404",
			statusCode: http.StatusNotFound,
			want:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
			}))
			defer server.Close()

			client := New(server.URL)
			got, err := client.Check(context.Background())
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

func TestCheckUnreachable(t *testing.T) {
	client := New("http://127.0.0.1:1/health")
	got, err := client.Check(context.Background())
	if err == nil {
		t.Fatalf("expected error for unreachable server, got nil")
	}
	if got {
		t.Fatalf("expected false for unreachable server")
	}
}
