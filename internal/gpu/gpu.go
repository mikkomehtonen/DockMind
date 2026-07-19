package gpu

import (
	"context"
	"os/exec"
	"strconv"
	"strings"

	"github.com/dockmind/dockmind/internal/state"
)

type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

type Monitor struct {
	exec execFunc
}

func New() *Monitor {
	return &Monitor{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			return cmd.Output()
		},
	}
}

func (m *Monitor) Status(ctx context.Context) (bool, string, error) {
	out, err := m.exec(ctx, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	if err != nil {
		return false, "", err
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return true, trimmed, nil
		}
	}
	return false, "", nil
}

func (m *Monitor) Processes(ctx context.Context) ([]state.GPUProcess, error) {
	out, err := m.exec(ctx, "nvidia-smi", "--query-compute-apps=pid,process_name", "--format=csv,noheader")
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(out), "\n")
	procs := make([]state.GPUProcess, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.SplitN(trimmed, ", ", 2)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		procs = append(procs, state.GPUProcess{
			PID:  pid,
			Name: fields[1],
		})
	}
	return procs, nil
}
