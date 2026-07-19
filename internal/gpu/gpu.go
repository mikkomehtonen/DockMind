package gpu

import (
	"context"
	"fmt"
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
	out, err := m.exec(ctx, "nvidia-smi", "--query-compute-apps=pid,process_name,used_gpu_memory", "--format=csv,noheader")
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
		firstSep := strings.Index(trimmed, ", ")
		if firstSep < 0 {
			continue
		}
		lastSep := strings.LastIndex(trimmed, ", ")
		if lastSep <= firstSep {
			continue
		}
		pidStr := strings.TrimSpace(trimmed[:firstSep])
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		procs = append(procs, state.GPUProcess{
			PID:           pid,
			Name:          trimmed[firstSep+2 : lastSep],
			UsedGPUMemory: trimmed[lastSep+2:],
		})
	}
	return procs, nil
}

func (m *Monitor) Memory(ctx context.Context) (state.GPUMemory, error) {
	out, err := m.exec(ctx, "nvidia-smi", "--query-gpu=memory.total,memory.used,memory.free", "--format=csv,noheader")
	if err != nil {
		return state.GPUMemory{}, err
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.SplitN(trimmed, ", ", 3)
		if len(fields) < 3 {
			return state.GPUMemory{}, fmt.Errorf("unexpected nvidia-smi memory output: %q", trimmed)
		}
		return state.GPUMemory{
			Total: fields[0],
			Used:  fields[1],
			Free:  fields[2],
		}, nil
	}
	return state.GPUMemory{}, fmt.Errorf("no nvidia-smi memory output")
}
