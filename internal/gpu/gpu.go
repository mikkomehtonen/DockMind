package gpu

import (
	"context"
	"os/exec"
	"strings"
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
