package gpu

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
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

func TestMemory(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		execErr error
		want    state.GPUMemory
		wantErr bool
	}{
		{
			name:   "single gpu",
			stdout: "16311 MiB, 12742 MiB, 3108 MiB, 24 %\n",
			want:   state.GPUMemory{Total: "16311 MiB", Used: "12742 MiB", Free: "3108 MiB", Utilization: "24 %"},
		},
		{
			name:   "idle gpu",
			stdout: "16311 MiB, 0 MiB, 16311 MiB, 0 %\n",
			want:   state.GPUMemory{Total: "16311 MiB", Used: "0 MiB", Free: "16311 MiB", Utilization: "0 %"},
		},
		{
			name:   "multiple gpus first line",
			stdout: "16311 MiB, 12742 MiB, 3108 MiB, 24 %\n24576 MiB, 0 MiB, 24576 MiB, 0 %\n",
			want:   state.GPUMemory{Total: "16311 MiB", Used: "12742 MiB", Free: "3108 MiB", Utilization: "24 %"},
		},
		{
			name:    "empty stdout",
			stdout:  "",
			wantErr: true,
		},
		{
			name:    "missing fields",
			stdout:  "16311 MiB, 12742 MiB, 3108 MiB\n",
			wantErr: true,
		},
		{
			name:    "exec error",
			stdout:  "",
			execErr: errors.New("nvidia-smi failed"),
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

			got, err := m.Memory(context.Background())
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

func TestMemoryQueryIncludesUtilization(t *testing.T) {
	var gotName string
	var gotArgs []string
	m := &Monitor{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			gotName = name
			gotArgs = args
			return []byte("16311 MiB, 12742 MiB, 3108 MiB, 24 %\n"), nil
		},
	}
	_, err := m.Memory(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotName != "nvidia-smi" {
		t.Fatalf("expected nvidia-smi, got %q", gotName)
	}
	if len(gotArgs) != 2 {
		t.Fatalf("expected 2 args, got %d: %v", len(gotArgs), gotArgs)
	}
	if gotArgs[0] != "--query-gpu=memory.total,memory.used,memory.free,utilization.gpu" {
		t.Fatalf("unexpected query arg: %q", gotArgs[0])
	}
	if gotArgs[1] != "--format=csv,noheader" {
		t.Fatalf("unexpected format arg: %q", gotArgs[1])
	}
	query := strings.Join(gotArgs, " ")
	if !strings.Contains(query, "utilization.gpu") {
		t.Fatalf("expected query to contain utilization.gpu, got %q", query)
	}
	if strings.Count(query, "utilization.gpu") != 1 {
		t.Fatalf("expected utilization.gpu exactly once, got %d", strings.Count(query, "utilization.gpu"))
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
			stdout: "11213, /app/.venv/bin/python3, 1234 MiB\n" +
				"16131, whisper-server, 567 MiB\n" +
				"82497, llama-server, 12734 MiB\n",
			want: []state.GPUProcess{
				{PID: 11213, Name: "/app/.venv/bin/python3", UsedGPUMemory: "1234 MiB"},
				{PID: 16131, Name: "whisper-server", UsedGPUMemory: "567 MiB"},
				{PID: 82497, Name: "llama-server", UsedGPUMemory: "12734 MiB"},
			},
		},
		{
			name:   "empty stdout",
			stdout: "",
			want:   []state.GPUProcess{},
		},
		{
			name:   "single process",
			stdout: "1234, some-process, 42 MiB\n",
			want:   []state.GPUProcess{{PID: 1234, Name: "some-process", UsedGPUMemory: "42 MiB"}},
		},
		{
			name:   "process name contains comma",
			stdout: "11213, my, process, 99 MiB\n",
			want:   []state.GPUProcess{{PID: 11213, Name: "my, process", UsedGPUMemory: "99 MiB"}},
		},
		{
			name:   "missing memory field",
			stdout: "11213, some-process\n",
			want:   []state.GPUProcess{},
		},
		{
			name:   "non-numeric pid",
			stdout: "abc, some-process, 1 MiB\n",
			want:   []state.GPUProcess{},
		},
		{
			name:   "N/A memory value",
			stdout: "1234, some-process, N/A\n",
			want:   []state.GPUProcess{{PID: 1234, Name: "some-process", UsedGPUMemory: "N/A"}},
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
