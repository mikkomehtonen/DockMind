package unbind

import (
	"context"
	"os/exec"
)

type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

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

func (c *Client) Unbind(ctx context.Context) error {
	_, err := c.exec(ctx, "sudo", "-n", "/usr/bin/systemctl", "start", "dockmind-egpu-unbind.service")
	return err
}
