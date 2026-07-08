package gpu

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
)

type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

type Monitor struct {
	exec   execFunc
	logger *slog.Logger
}

func New(logger *slog.Logger) *Monitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Monitor{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			return cmd.Output()
		},
		logger: logger,
	}
}

func (m *Monitor) Status(ctx context.Context) (bool, string, error) {
	out, err := m.exec(ctx, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	if err != nil {
		// Per MVP spec, any nvidia-smi failure is treated as GPU absent.
		if m.logger != nil {
			m.logger.Warn("nvidia-smi failed", "error", err)
		}
		return false, "", nil
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
