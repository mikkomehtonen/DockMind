package docker

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

func TestStart(t *testing.T) {
	cases := []struct {
		name      string
		container string
		exitErr   bool
		execErr   error
		wantErr   bool
		wantArgs  []string
	}{
		{
			name:      "start success",
			container: "llama-swap",
			wantArgs:  []string{"docker", "start", "llama-swap"},
		},
		{
			name:      "custom container",
			container: "my-runner",
			wantArgs:  []string{"docker", "start", "my-runner"},
		},
		{
			name:      "start error",
			container: "llama-swap",
			exitErr:   true,
			wantErr:   true,
			wantArgs:  []string{"docker", "start", "llama-swap"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			c := &Client{
				container: tc.container,
				exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
					gotArgs = append([]string{name}, args...)
					if tc.execErr != nil {
						return nil, tc.execErr
					}
					if tc.exitErr {
						return nil, &exec.ExitError{Stderr: []byte("error starting container")}
					}
					return []byte(""), nil
				},
			}

			err := c.Start(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Fatalf("expected args %v, got %v", tc.wantArgs, gotArgs)
			}
		})
	}
}

func TestStop(t *testing.T) {
	cases := []struct {
		name      string
		container string
		exitErr   bool
		stderr    string
		wantErr   bool
		wantArgs  []string
	}{
		{
			name:      "stop success",
			container: "llama-swap",
			wantArgs:  []string{"docker", "stop", "llama-swap"},
		},
		{
			name:      "no such container",
			container: "llama-swap",
			exitErr:   true,
			stderr:    "Error: No such container: llama-swap",
			wantArgs:  []string{"docker", "stop", "llama-swap"},
		},
		{
			name:      "other error",
			container: "llama-swap",
			exitErr:   true,
			stderr:    "docker daemon not running",
			wantErr:   true,
			wantArgs:  []string{"docker", "stop", "llama-swap"},
		},
		{
			name:      "custom container",
			container: "my-runner",
			wantArgs:  []string{"docker", "stop", "my-runner"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			c := &Client{
				container: tc.container,
				exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
					gotArgs = append([]string{name}, args...)
					if tc.exitErr {
						return nil, &exec.ExitError{Stderr: []byte(tc.stderr)}
					}
					return []byte(""), nil
				},
			}

			err := c.Stop(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Fatalf("expected args %v, got %v", tc.wantArgs, gotArgs)
			}
		})
	}
}

func TestIsRunning(t *testing.T) {
	cases := []struct {
		name      string
		container string
		stdout    string
		exitErr   bool
		want      bool
		wantErr   bool
		wantArgs  []string
	}{
		{
			name:      "running true",
			container: "llama-swap",
			stdout:    "true",
			want:      true,
			wantArgs:  []string{"docker", "inspect", "--format", "{{.State.Running}}", "llama-swap"},
		},
		{
			name:      "running false",
			container: "llama-swap",
			stdout:    "false",
			want:      false,
			wantArgs:  []string{"docker", "inspect", "--format", "{{.State.Running}}", "llama-swap"},
		},
		{
			name:      "container not found",
			container: "llama-swap",
			exitErr:   true,
			want:      false,
			wantArgs:  []string{"docker", "inspect", "--format", "{{.State.Running}}", "llama-swap"},
		},
		{
			name:      "custom container",
			container: "my-runner",
			stdout:    "true",
			want:      true,
			wantArgs:  []string{"docker", "inspect", "--format", "{{.State.Running}}", "my-runner"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			c := &Client{
				container: tc.container,
				exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
					gotArgs = append([]string{name}, args...)
					if tc.exitErr {
						return nil, &exec.ExitError{Stderr: []byte("No such container")}
					}
					return []byte(tc.stdout), nil
				},
			}

			got, err := c.IsRunning(context.Background())
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
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Fatalf("expected args %v, got %v", tc.wantArgs, gotArgs)
			}
		})
	}
}

func TestIsRunningExecError(t *testing.T) {
	c := &Client{
		container: "llama-swap",
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("exec error")
		},
	}
	got, err := c.IsRunning(context.Background())
	if err == nil {
		t.Fatalf("expected error on exec error")
	}
	if got {
		t.Fatalf("expected false on exec error")
	}
}
