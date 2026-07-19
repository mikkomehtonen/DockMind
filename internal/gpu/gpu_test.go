package gpu

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"

	"github.com/dockmind/dockmind/internal/state"
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
		wantErr     bool
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
			wantErr:     true,
		},
		{
			name:        "non-zero exit",
			exitErr:     true,
			wantPresent: false,
			wantName:    "",
			wantErr:     true,
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
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
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

func TestStatusReturnsExecError(t *testing.T) {
	m := &Monitor{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("some error")
		},
	}
	present, name, err := m.Status(context.Background())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if present || name != "" {
		t.Fatalf("expected absent GPU on error")
	}
}

func TestNew(t *testing.T) {
	m := New()
	if m == nil {
		t.Fatalf("expected non-nil Monitor")
	}
	if m.exec == nil {
		t.Fatalf("expected default execFunc")
	}
}

func TestProcesses(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		execErr error
		want    []state.GPUProcess
		wantErr bool
	}{
		{
			name: "three processes",
			stdout: "11213, /app/.venv/bin/python3\n" +
				"16131, whisper-server\n" +
				"82497, llama-server\n",
			want: []state.GPUProcess{
				{PID: 11213, Name: "/app/.venv/bin/python3"},
				{PID: 16131, Name: "whisper-server"},
				{PID: 82497, Name: "llama-server"},
			},
		},
		{
			name:   "empty stdout",
			stdout: "",
			want:   []state.GPUProcess{},
		},
		{
			name:   "single process",
			stdout: "1234, some-process\n",
			want:   []state.GPUProcess{{PID: 1234, Name: "some-process"}},
		},
		{
			name:   "process name contains comma",
			stdout: "11213, my, process\n",
			want:   []state.GPUProcess{{PID: 11213, Name: "my, process"}},
		},
		{
			name:   "missing name field",
			stdout: "11213\n",
			want:   []state.GPUProcess{},
		},
		{
			name:   "non-numeric pid",
			stdout: "abc, some-process\n",
			want:   []state.GPUProcess{},
		},
		{
			name:    "exec error",
			stdout:  "",
			execErr: errors.New("nvidia-smi failed"),
			want:    nil,
			wantErr: true,
		},
		{
			name:    "exit error",
			stdout:  "",
			execErr: &exec.ExitError{Stderr: []byte("error")},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Monitor{
				exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
					if tc.execErr != nil {
						return nil, tc.execErr
					}
					return []byte(tc.stdout), nil
				},
			}

			got, err := m.Processes(context.Background())
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("expected %+v, got %+v", tc.want, got)
			}
		})
	}
}
