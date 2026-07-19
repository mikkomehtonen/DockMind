package docker

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

var ErrUnknownContainer = errors.New("unknown container")

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

type ContainerSpec struct {
	Name      string
	Container string
}

type Manager struct {
	specs []ContainerSpec
	ctrls map[string]*Client
}

func NewManager(specs []ContainerSpec) *Manager {
	m := &Manager{
		specs: make([]ContainerSpec, 0, len(specs)),
		ctrls: make(map[string]*Client, len(specs)),
	}
	for _, spec := range specs {
		m.specs = append(m.specs, spec)
		m.ctrls[spec.Name] = New(spec.Container)
	}
	return m
}

// SetExec replaces the exec function used by all managed clients. It is
// intended for tests that need to inject a fake command executor.
func (m *Manager) SetExec(exec execFunc) {
	for _, c := range m.ctrls {
		c.exec = exec
	}
}

func (m *Manager) Names() []string {
	names := make([]string, len(m.specs))
	for i, spec := range m.specs {
		names[i] = spec.Name
	}
	return names
}

func (m *Manager) Start(ctx context.Context, name string) error {
	c, ok := m.ctrls[name]
	if !ok {
		return ErrUnknownContainer
	}
	return c.Start(ctx)
}

func (m *Manager) Stop(ctx context.Context, name string) error {
	c, ok := m.ctrls[name]
	if !ok {
		return ErrUnknownContainer
	}
	return c.Stop(ctx)
}

func (m *Manager) IsRunning(ctx context.Context, name string) (bool, error) {
	c, ok := m.ctrls[name]
	if !ok {
		return false, ErrUnknownContainer
	}
	return c.IsRunning(ctx)
}

func (m *Manager) StopAll(ctx context.Context) error {
	for _, spec := range m.specs {
		if err := m.ctrls[spec.Name].Stop(ctx); err != nil {
			return err
		}
	}
	return nil
}
