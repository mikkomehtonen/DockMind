package lact

import (
	"context"
	"os/exec"
)

type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// Client controls the LACT daemon (lactd) systemd service lifecycle. LACT
// loses its ability to detect an eGPU permanently if it is running while the
// eGPU is unbound, so DockMind stops lactd before unbinding and starts it
// again once the GPU driver is back.
type Client struct {
	exec execFunc
}

func New() *Client {
	return &Client{
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			return cmd.Output()
		},
	}
}

// SetExec replaces the command executor used for all internal calls. It is
// intended for tests.
func (c *Client) SetExec(fn execFunc) {
	c.exec = fn
}

// Stop stops the lactd systemd service.
func (c *Client) Stop(ctx context.Context) error {
	_, err := c.exec(ctx, "sudo", "-n", "/usr/bin/systemctl", "stop", "lactd")
	return err
}

// Start starts the lactd systemd service.
func (c *Client) Start(ctx context.Context) error {
	_, err := c.exec(ctx, "sudo", "-n", "/usr/bin/systemctl", "start", "lactd")
	return err
}
