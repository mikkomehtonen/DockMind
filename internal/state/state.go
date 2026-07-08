package state

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type State int

const (
	Off State = iota
	Starting
	Ready
	ShuttingDown
	Error
)

func (s State) String() string {
	switch s {
	case Off:
		return "Off"
	case Starting:
		return "Starting"
	case Ready:
		return "Ready"
	case ShuttingDown:
		return "ShuttingDown"
	case Error:
		return "Error"
	default:
		return "Unknown"
	}
}

type PowerResult int

const (
	ResultAccepted PowerResult = iota
	ResultAlreadyInState
	ResultConflict
)

type PowerController interface {
	SetPower(ctx context.Context, on bool) error
	IsOn(ctx context.Context) (bool, error)
}

type GPUMonitor interface {
	Status(ctx context.Context) (present bool, name string, err error)
}

type ContainerController interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	IsRunning(ctx context.Context) (bool, error)
}

type HealthChecker interface {
	Check(ctx context.Context) (healthy bool, err error)
}

type StatusResponse struct {
	State            string  `json:"state"`
	GPUPresent       bool    `json:"gpuPresent"`
	GPUName          string  `json:"gpuName"`
	ShellyOn         bool    `json:"shellyOn"`
	LlamaSwapRunning bool    `json:"llamaSwapRunning"`
	LlamaSwapHealthy bool    `json:"llamaSwapHealthy"`
	LastError        *string `json:"lastError"`
}

type Machine struct {
	power  PowerController
	gpu    GPUMonitor
	docker ContainerController
	health HealthChecker
	logger *slog.Logger

	pollInterval    time.Duration
	startupTimeout  time.Duration
	shutdownTimeout time.Duration

	transitionMu sync.Mutex
	stateMu      sync.Mutex
	state        State
	lastError    error
	wg           sync.WaitGroup
}

func New(power PowerController, gpu GPUMonitor, docker ContainerController, health HealthChecker, logger *slog.Logger, pollInterval, startupTimeout, shutdownTimeout time.Duration) *Machine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Machine{
		power:           power,
		gpu:             gpu,
		docker:          docker,
		health:          health,
		logger:          logger,
		pollInterval:    pollInterval,
		startupTimeout:  startupTimeout,
		shutdownTimeout: shutdownTimeout,
		state:           Off,
	}
}

func (m *Machine) PowerOn() PowerResult {
	if !m.transitionMu.TryLock() {
		return ResultConflict
	}

	m.stateMu.Lock()
	current := m.state
	m.stateMu.Unlock()

	switch current {
	case Ready:
		m.transitionMu.Unlock()
		return ResultAlreadyInState
	case Starting, ShuttingDown, Error:
		m.transitionMu.Unlock()
		return ResultConflict
	case Off:
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer m.transitionMu.Unlock()
			m.startup()
		}()
		return ResultAccepted
	default:
		m.transitionMu.Unlock()
		return ResultConflict
	}
}

func (m *Machine) PowerOff() PowerResult {
	if !m.transitionMu.TryLock() {
		return ResultConflict
	}

	m.stateMu.Lock()
	current := m.state
	m.stateMu.Unlock()

	switch current {
	case Off:
		m.transitionMu.Unlock()
		return ResultAlreadyInState
	case Starting, ShuttingDown:
		m.transitionMu.Unlock()
		return ResultConflict
	case Ready, Error:
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer m.transitionMu.Unlock()
			m.shutdown()
		}()
		return ResultAccepted
	default:
		m.transitionMu.Unlock()
		return ResultConflict
	}
}

func (m *Machine) Restart() PowerResult {
	if !m.transitionMu.TryLock() {
		return ResultConflict
	}

	m.stateMu.Lock()
	current := m.state
	m.stateMu.Unlock()

	switch current {
	case Starting, ShuttingDown, Error:
		m.transitionMu.Unlock()
		return ResultConflict
	case Off, Ready:
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer m.transitionMu.Unlock()
			m.restart()
		}()
		return ResultAccepted
	default:
		m.transitionMu.Unlock()
		return ResultConflict
	}
}

func (m *Machine) Status() StatusResponse {
	m.stateMu.Lock()
	state := m.state
	lastErr := m.lastError
	m.stateMu.Unlock()

	gpuPresent, gpuName := m.probeGPU()
	shellyOn := m.probeBool("Shelly", func(ctx context.Context) (bool, error) {
		return m.power.IsOn(ctx)
	})
	running := m.probeBool("Docker", func(ctx context.Context) (bool, error) {
		return m.docker.IsRunning(ctx)
	})
	healthy := m.probeBool("Health", func(ctx context.Context) (bool, error) {
		return m.health.Check(ctx)
	})

	var lastError *string
	if lastErr != nil {
		s := lastErr.Error()
		lastError = &s
	}

	return StatusResponse{
		State:            state.String(),
		GPUPresent:       gpuPresent,
		GPUName:          gpuName,
		ShellyOn:         shellyOn,
		LlamaSwapRunning: running,
		LlamaSwapHealthy: healthy,
		LastError:        lastError,
	}
}

func (m *Machine) probeGPU() (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	present, name, err := m.gpu.Status(ctx)
	if err != nil {
		m.logger.Warn("GPU status probe failed", "error", err)
	}
	return present, name
}

func (m *Machine) probeBool(name string, fn func(context.Context) (bool, error)) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	val, err := fn(ctx)
	if err != nil {
		m.logger.Warn(name+" status probe failed", "error", err)
	}
	return val
}

func (m *Machine) Wait() {
	m.wg.Wait()
}

func (m *Machine) setState(s State, err error) {
	m.stateMu.Lock()
	m.state = s
	m.lastError = err
	m.stateMu.Unlock()
}

func (m *Machine) startup() {
	m.setState(Starting, nil)
	m.logger.Info("State -> Starting")

	ctx, cancel := context.WithTimeout(context.Background(), m.startupTimeout)
	defer cancel()

	m.logger.Info("Shelly power ON")
	if err := m.power.SetPower(ctx, true); err != nil {
		m.setState(Error, fmt.Errorf("shelly power on failed: %w", err))
		m.logger.Error("Shelly power ON failed", "error", err)
		return
	}

	m.logger.Info("Waiting for GPU")
	if err := m.poll(ctx, m.pollInterval, func(ctx context.Context) (bool, error) {
		present, _, err := m.gpu.Status(ctx)
		return present, err
	}); err != nil {
		m.setState(Error, fmt.Errorf("gpu detection timeout: %w", err))
		m.logger.Error("GPU detection timeout", "error", err)
		return
	}
	m.logger.Info("GPU detected")

	m.logger.Info("Starting llama-swap")
	if err := m.docker.Start(ctx); err != nil {
		m.setState(Error, fmt.Errorf("docker start failed: %w", err))
		m.logger.Error("Docker start failed", "error", err)
		return
	}

	m.logger.Info("Waiting for llama-swap health")
	if err := m.poll(ctx, m.pollInterval, func(ctx context.Context) (bool, error) {
		return m.health.Check(ctx)
	}); err != nil {
		m.setState(Error, fmt.Errorf("llama-swap health check timeout: %w", err))
		m.logger.Error("llama-swap health check timeout", "error", err)
		return
	}

	m.setState(Ready, nil)
	m.logger.Info("State -> Ready")
}

func (m *Machine) shutdown() {
	m.setState(ShuttingDown, nil)
	m.logger.Info("State -> ShuttingDown")

	ctx, cancel := context.WithTimeout(context.Background(), m.shutdownTimeout)
	defer cancel()

	m.logger.Info("Stopping llama-swap")
	if err := m.docker.Stop(ctx); err != nil {
		m.setState(Error, fmt.Errorf("docker stop failed: %w", err))
		m.logger.Error("Docker stop failed", "error", err)
		return
	}

	m.logger.Info("Waiting for llama-swap to stop")
	if err := m.poll(ctx, m.pollInterval, func(ctx context.Context) (bool, error) {
		running, err := m.docker.IsRunning(ctx)
		return !running, err
	}); err != nil {
		m.setState(Error, fmt.Errorf("llama-swap stop timeout: %w", err))
		m.logger.Error("llama-swap stop timeout", "error", err)
		return
	}

	m.logger.Info("Shelly power OFF")
	if err := m.power.SetPower(ctx, false); err != nil {
		m.setState(Error, fmt.Errorf("shelly power off failed: %w", err))
		m.logger.Error("Shelly power OFF failed", "error", err)
		return
	}

	m.logger.Info("Waiting for GPU to disappear")
	if err := m.poll(ctx, m.pollInterval, func(ctx context.Context) (bool, error) {
		present, _, err := m.gpu.Status(ctx)
		return !present, err
	}); err != nil {
		m.setState(Error, fmt.Errorf("gpu power off timeout: %w", err))
		m.logger.Error("GPU power off timeout", "error", err)
		return
	}

	m.setState(Off, nil)
	m.logger.Info("State -> Off")
}

func (m *Machine) restart() {
	m.stateMu.Lock()
	current := m.state
	m.stateMu.Unlock()

	if current == Ready {
		m.shutdown()
	}

	m.stateMu.Lock()
	current = m.state
	m.stateMu.Unlock()
	if current == Error {
		return
	}

	m.startup()
}

func (m *Machine) poll(ctx context.Context, interval time.Duration, check func(context.Context) (bool, error)) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
	for {
		ok, err := check(ctx)
		if err == nil && ok {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w (last check error: %v)", ctx.Err(), lastErr)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
