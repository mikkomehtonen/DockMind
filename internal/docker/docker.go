package docker

import (
	"context"
	"os/exec"
	"strings"
)

type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

type Client struct {
	container string
	exec      execFunc
}

func New(container string) *Client {
	return &Client{
		container: container,
		exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			return cmd.Output()
		},
	}
}

func (c *Client) Start(ctx context.Context) error {
	_, err := c.exec(ctx, "docker", "start", c.container)
	return err
}

func (c *Client) Stop(ctx context.Context) error {
	_, err := c.exec(ctx, "docker", "stop", c.container)
	if err == nil {
		return nil
	}
	exitErr, ok := err.(*exec.ExitError)
	if ok && strings.Contains(string(exitErr.Stderr), "No such container") {
		return nil
	}
	return err
}

func (c *Client) IsRunning(ctx context.Context) (bool, error) {
	out, err := c.exec(ctx, "docker", "inspect", "--format", "{{.State.Running}}", c.container)
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if ok && strings.Contains(string(exitErr.Stderr), "No such container") {
			return false, nil
		}
		return false, err
	}
	s := strings.TrimSpace(string(out))
	return s == "true", nil
}
