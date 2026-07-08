package gpu

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestStatus(t *testing.T) {
	cases := []struct {
		name        string
		stdout      string
		stderr      string
		exitErr     bool
		execErr     error
		wantPresent bool
		wantName    string
	}{
		{
			name:        "single gpu",
			stdout:      "NVIDIA GeForce RTX 5060 Ti",
			wantPresent: true,
			wantName:    "NVIDIA GeForce RTX 5060 Ti",
		},
		{
			name:        "multiple gpus first line",
			stdout:      "NVIDIA GeForce RTX 5060 Ti\nNVIDIA GeForce RTX 4090",
			wantPresent: true,
			wantName:    "NVIDIA GeForce RTX 5060 Ti",
		},
		{
			name:        "empty stdout",
			stdout:      "",
			wantPresent: false,
			wantName:    "",
		},
		{
			name:        "binary not found",
			execErr:     &exec.Error{Name: "nvidia-smi", Err: exec.ErrNotFound},
			wantPresent: false,
			wantName:    "",
		},
		{
			name:        "non-zero exit",
			exitErr:     true,
			wantPresent: false,
			wantName:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Monitor{
				exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
					if tc.execErr != nil {
						return nil, tc.execErr
					}
					if tc.exitErr {
						return []byte(tc.stdout), &exec.ExitError{Stderr: []byte(tc.stderr)}
					}
					return []byte(tc.stdout), nil
				},
			}

			present, name, err := m.Status(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if present != tc.wantPresent {
				t.Fatalf("expected present=%v, got %v", tc.wantPresent, present)
			}
			if name != tc.wantName {
				t.Fatalf("expected name=%q, got %q", tc.wantName, name)
			}
		})
	}
}

func TestStatusSwallowsExecError(t *testing.T) {
	m := &Monitor{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("some error")
		},
	}
	present, name, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if present || name != "" {
		t.Fatalf("expected absent GPU on error")
	}
}
