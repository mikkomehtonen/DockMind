package unbind

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestClientUnbindExecutesExpectedCommand(t *testing.T) {
	var gotArgs []string
	c := &Client{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			gotArgs = append([]string{name}, args...)
			return nil, nil
		},
	}

	if err := c.Unbind(context.Background()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	want := []string{"sudo", "-n", "/usr/bin/systemctl", "start", "dockmind-egpu-unbind.service"}
	if len(gotArgs) != len(want) {
		t.Fatalf("expected args %v, got %v", want, gotArgs)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("expected arg %d %q, got %q", i, want[i], gotArgs[i])
		}
	}
}

func TestClientUnbindReturnsNilOnSuccess(t *testing.T) {
	c := &Client{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, nil
		},
	}

	if err := c.Unbind(context.Background()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestClientUnbindReturnsErrorOnGenericError(t *testing.T) {
	wantErr := errors.New("sudo failed")
	c := &Client{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, wantErr
		},
	}

	if err := c.Unbind(context.Background()); err != wantErr {
		t.Fatalf("expected error %v, got %v", wantErr, err)
	}
}

func TestClientUnbindReturnsExitError(t *testing.T) {
	wantErr := &exec.ExitError{}
	c := &Client{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, wantErr
		},
	}

	err := c.Unbind(context.Background())
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %T", err)
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
