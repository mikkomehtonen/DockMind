package lact

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestClientCommands(t *testing.T) {
	cases := []struct {
		name string
		call func(*Client, context.Context) error
		want []string
	}{
		{
			name: "Stop passes stop lactd to systemctl",
			call: func(c *Client, ctx context.Context) error { return c.Stop(ctx) },
			want: []string{"sudo", "-n", "/usr/bin/systemctl", "stop", "lactd"},
		},
		{
			name: "Start passes start lactd to systemctl",
			call: func(c *Client, ctx context.Context) error { return c.Start(ctx) },
			want: []string{"sudo", "-n", "/usr/bin/systemctl", "start", "lactd"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			c := &Client{
				exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
					gotArgs = append([]string{name}, args...)
					return nil, nil
				},
			}

			if err := tc.call(c, context.Background()); err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}

			if len(gotArgs) != len(tc.want) {
				t.Fatalf("expected args %v, got %v", tc.want, gotArgs)
			}
			for i := range tc.want {
				if gotArgs[i] != tc.want[i] {
					t.Fatalf("expected arg %d %q, got %q", i, tc.want[i], gotArgs[i])
				}
			}
		})
	}
}

func TestClientReturnsNilOnSuccess(t *testing.T) {
	c := &Client{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, nil
		},
	}

	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestClientReturnsErrorOnFailure(t *testing.T) {
	wantErr := errors.New("sudo failed")
	c := &Client{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, wantErr
		},
	}

	if err := c.Stop(context.Background()); err != wantErr {
		t.Fatalf("expected error %v, got %v", wantErr, err)
	}
	if err := c.Start(context.Background()); err != wantErr {
		t.Fatalf("expected error %v, got %v", wantErr, err)
	}
}

func TestClientReturnsExitError(t *testing.T) {
	wantErr := &exec.ExitError{}
	c := &Client{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, wantErr
		},
	}

	for _, call := range []func(context.Context) error{c.Stop, c.Start} {
		err := call(context.Background())
		if err == nil {
			t.Fatal("expected non-nil error")
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("expected *exec.ExitError, got %T", err)
		}
	}
}

func TestNewReturnsClientWithDefaultExec(t *testing.T) {
	c := New()
	if c == nil {
		t.Fatal("expected non-nil *Client")
	}
	if c.exec == nil {
		t.Fatal("expected default execFunc to be set")
	}
}

func TestSetExecReplacesExec(t *testing.T) {
	c := New()
	var gotArgs []string
	c.SetExec(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		return nil, nil
	})

	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	want := []string{"sudo", "-n", "/usr/bin/systemctl", "stop", "lactd"}
	if len(gotArgs) != len(want) {
		t.Fatalf("expected args %v, got %v", want, gotArgs)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("expected arg %d %q, got %q", i, want[i], gotArgs[i])
		}
	}
}
