package state

import (
	"context"
	"errors"
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
	AwaitingGPUFree
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
	case AwaitingGPUFree:
		return "AwaitingGPUFree"
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
	ResultCooldown
)

var ErrBackendError = errors.New("backend in error state")

type PowerController interface {
	SetPower(ctx context.Context, on bool) error
	IsOn(ctx context.Context) (bool, error)
}

type GPUProcess struct {
	PID           int    `json:"pid"`
	Name          string `json:"name"`
	UsedGPUMemory string `json:"usedGpuMemory"`
}

type GPUMemory struct {
	Total string `json:"total"`
	Used  string `json:"used"`
	Free  string `json:"free"`
}

type GPUMonitor interface {
	Status(ctx context.Context) (present bool, name string, err error)
	Processes(ctx context.Context) ([]GPUProcess, error)
	Memory(ctx context.Context) (GPUMemory, error)
}

type ContainerController interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	IsRunning(ctx context.Context) (bool, error)
}

type HealthChecker interface {
	Check(ctx context.Context) (healthy bool, models []string, err error)
}

type Unbinder interface {
	Unbind(ctx context.Context) error
}

type AuxResult int

const (
	AuxResultOK AuxResult = iota
	AuxResultNotFound
	AuxResultConflict
	AuxResultError
)

type AuxContainerStatus struct {
	Name    string `json:"name"`
	Running bool   `json:"running"`
}

type AuxContainerController interface {
	Names() []string
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	IsRunning(ctx context.Context, name string) (bool, error)
	StopAll(ctx context.Context) error
}

type StatusResponse struct {
	State             string               `json:"state"`
	GPUPresent        bool                 `json:"gpuPresent"`
	GPUName           string               `json:"gpuName"`
	ShellyOn          bool                 `json:"shellyOn"`
	LlamaSwapRunning  bool                 `json:"llamaSwapRunning"`
	LlamaSwapHealthy  bool                 `json:"llamaSwapHealthy"`
	LoadedModels      []string             `json:"loadedModels"`
	GPUProcesses      []GPUProcess         `json:"gpuProcesses"`
	GPUMemory         GPUMemory            `json:"gpuMemory"`
	LastError         *string              `json:"lastError"`
	CooldownRemaining float64              `json:"cooldownRemaining"`
	IdleRemaining     float64              `json:"idleRemaining"`
	AuxContainers     []AuxContainerStatus `json:"auxContainers"`
}

type Machine struct {
	power    PowerController
	gpu      GPUMonitor
	docker   ContainerController
	health   HealthChecker
	unbinder Unbinder
	aux      AuxContainerController
	logger   *slog.Logger

	pollInterval         time.Duration
	startupTimeout       time.Duration
	shutdownTimeout      time.Duration
	gpuFreeCheckInterval time.Duration
	cooldown             time.Duration

	transitionMu  sync.Mutex
	stateMu       sync.Mutex
	state         State
	lastError     error
	lastReadyTime time.Time
	lastOffTime   time.Time
	resumeStartup bool
	resumeCh      chan struct{}
	changeCh      chan struct{}
	wg            sync.WaitGroup
}

func New(power PowerController, gpu GPUMonitor, docker ContainerController, health HealthChecker, unbinder Unbinder, logger *slog.Logger, pollInterval, startupTimeout, shutdownTimeout, gpuFreeCheckInterval, cooldown time.Duration) *Machine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Machine{
		power:                power,
		gpu:                  gpu,
		docker:               docker,
		health:               health,
		unbinder:             unbinder,
		logger:               logger,
		pollInterval:         pollInterval,
		startupTimeout:       startupTimeout,
		shutdownTimeout:      shutdownTimeout,
		gpuFreeCheckInterval: gpuFreeCheckInterval,
		cooldown:             cooldown,
		state:                Off,
		changeCh:             make(chan struct{}),
	}
}

func (m *Machine) SetAuxContainers(ctrl AuxContainerController) {
	m.aux = ctrl
}

func (m *Machine) PowerOn() PowerResult {
	m.stateMu.Lock()
	current := m.state
	m.stateMu.Unlock()
	if current == AwaitingGPUFree {
		m.requestResume()
		return ResultAccepted
	}

	if !m.transitionMu.TryLock() {
		return ResultConflict
	}

	m.stateMu.Lock()
	current = m.state
	inCooldown := m.cooldownActiveLocked(Off)
	m.stateMu.Unlock()

	switch current {
	case Ready:
		m.transitionMu.Unlock()
		return ResultAlreadyInState
	case Starting, ShuttingDown, Error:
		m.transitionMu.Unlock()
		return ResultConflict
	case Off:
		if inCooldown {
			m.transitionMu.Unlock()
			return ResultCooldown
		}
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
	m.stateMu.Lock()
	current := m.state
	m.stateMu.Unlock()
	if current == AwaitingGPUFree {
		return ResultAlreadyInState
	}

	if !m.transitionMu.TryLock() {
		return ResultConflict
	}

	m.stateMu.Lock()
	current = m.state
	inCooldown := m.cooldownActiveLocked(Ready)
	m.stateMu.Unlock()

	switch current {
	case Off:
		m.transitionMu.Unlock()
		return ResultAlreadyInState
	case Starting, ShuttingDown:
		m.transitionMu.Unlock()
		return ResultConflict
	case Ready:
		if inCooldown {
			m.transitionMu.Unlock()
			return ResultCooldown
		}
		fallthrough
	case Error:
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
	m.stateMu.Lock()
	current := m.state
	m.stateMu.Unlock()
	if current == AwaitingGPUFree {
		m.requestResume()
		return ResultAccepted
	}

	if !m.transitionMu.TryLock() {
		return ResultConflict
	}

	m.stateMu.Lock()
	current = m.state
	inCooldown := m.cooldownActiveLocked(current)
	m.stateMu.Unlock()

	switch current {
	case Starting, ShuttingDown, Error:
		m.transitionMu.Unlock()
		return ResultConflict
	case Off, Ready:
		if inCooldown {
			m.transitionMu.Unlock()
			return ResultCooldown
		}
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

	cooldownRemaining := m.cooldownRemainingLocked(state).Seconds()
	m.stateMu.Unlock()

	gpuPresent, gpuName := m.probeGPU(probeFailureExpected(state))
	var gpuProcesses []GPUProcess
	var gpuMemory GPUMemory
	if gpuPresent {
		gpuProcesses = m.probeGPUProcesses()
		if gpuProcesses != nil && len(gpuProcesses) > 0 {
			gpuMemory = m.probeGPUMemory()
		}
	}
	if gpuProcesses == nil {
		gpuProcesses = []GPUProcess{}
	}
	shellyOn := m.probeBool("Shelly", false, func(ctx context.Context) (bool, error) {
		return m.power.IsOn(ctx)
	})
	running := m.probeBool("Docker", false, func(ctx context.Context) (bool, error) {
		return m.docker.IsRunning(ctx)
	})
	healthy, models := m.probeHealth(probeFailureExpected(state))
	if models == nil {
		models = []string{}
	}

	var lastError *string
	if lastErr != nil {
		s := lastErr.Error()
		lastError = &s
	}

	auxStatuses := m.probeAuxContainers()

	return StatusResponse{
		State:             state.String(),
		GPUPresent:        gpuPresent,
		GPUName:           gpuName,
		ShellyOn:          shellyOn,
		LlamaSwapRunning:  running,
		LlamaSwapHealthy:  healthy,
		LoadedModels:      models,
		GPUProcesses:      gpuProcesses,
		GPUMemory:         gpuMemory,
		LastError:         lastError,
		CooldownRemaining: cooldownRemaining,
		AuxContainers:     auxStatuses,
	}
}

func (m *Machine) probeAuxContainers() []AuxContainerStatus {
	if m.aux == nil {
		return []AuxContainerStatus{}
	}

	var statuses []AuxContainerStatus
	for _, name := range m.aux.Names() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		running, err := m.aux.IsRunning(ctx, name)
		cancel()
		if err != nil {
			m.logger.Warn("Aux container status probe failed", "name", name, "error", err)
			running = false
		}
		statuses = append(statuses, AuxContainerStatus{Name: name, Running: running})
	}
	return statuses
}

func (m *Machine) StartAuxContainer(name string) AuxResult {
	return m.doAuxOperation(name, true)
}

func (m *Machine) StopAuxContainer(name string) AuxResult {
	return m.doAuxOperation(name, false)
}

func (m *Machine) doAuxOperation(name string, start bool) AuxResult {
	if m.aux == nil {
		return AuxResultNotFound
	}

	found := false
	for _, n := range m.aux.Names() {
		if n == name {
			found = true
			break
		}
	}
	if !found {
		return AuxResultNotFound
	}

	if !m.transitionMu.TryLock() {
		return AuxResultConflict
	}
	defer m.transitionMu.Unlock()

	m.stateMu.Lock()
	current := m.state
	m.stateMu.Unlock()

	if start {
		if current != Ready {
			return AuxResultConflict
		}
	} else {
		if current != Off && current != Ready {
			return AuxResultConflict
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var err error
	if start {
		err = m.aux.Start(ctx, name)
	} else {
		err = m.aux.Stop(ctx, name)
	}
	if err != nil {
		m.logger.Error("Aux container operation failed", "name", name, "start", start, "error", err)
		return AuxResultError
	}

	return AuxResultOK
}

func (m *Machine) probeGPU(quietOnFailure bool) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	present, name, err := m.gpu.Status(ctx)
	if err != nil {
		if quietOnFailure {
			m.logger.Debug("GPU status probe failed", "error", err)
		} else {
			m.logger.Warn("GPU status probe failed", "error", err)
		}
	}
	return present, name
}

func (m *Machine) probeBool(name string, quietOnFailure bool, fn func(context.Context) (bool, error)) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	val, err := fn(ctx)
	if err != nil {
		if quietOnFailure {
			m.logger.Debug(name+" status probe failed", "error", err)
		} else {
			m.logger.Warn(name+" status probe failed", "error", err)
		}
	}
	return val
}

func (m *Machine) probeHealth(quietOnFailure bool) (bool, []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	healthy, models, err := m.health.Check(ctx)
	if err != nil {
		if quietOnFailure {
			m.logger.Debug("Health status probe failed", "error", err)
		} else {
			m.logger.Warn("Health status probe failed", "error", err)
		}
		return false, nil
	}
	return healthy, models
}

func (m *Machine) probeGPUProcesses() []GPUProcess {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	procs, err := m.gpu.Processes(ctx)
	if err != nil {
		m.logger.Debug("GPU processes probe failed", "error", err)
		return nil
	}
	return procs
}

func (m *Machine) probeGPUMemory() GPUMemory {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mem, err := m.gpu.Memory(ctx)
	if err != nil {
		m.logger.Debug("GPU memory probe failed", "error", err)
		return GPUMemory{}
	}
	return mem
}

func probeFailureExpected(s State) bool {
	return s == Off || s == Starting || s == ShuttingDown || s == AwaitingGPUFree
}

func (m *Machine) Wait() {
	m.wg.Wait()
}

func (m *Machine) setState(s State, err error) {
	m.stateMu.Lock()
	m.state = s
	m.lastError = err
	if s == Ready {
		m.lastReadyTime = time.Now()
	} else if s == Off {
		m.lastOffTime = time.Now()
	}
	close(m.changeCh)
	m.changeCh = make(chan struct{})
	m.stateMu.Unlock()
}

// setStateOrResume atomically checks for a pending resume request and either
// returns true (caller should run startup) or transitions to s and returns
// false. This closes the race where PowerOn/Restart arrives after a "GPU free"
// decision but before the state transition.
func (m *Machine) setStateOrResume(s State) bool {
	m.stateMu.Lock()
	resume := m.resumeStartup
	m.resumeStartup = false
	ch := m.resumeCh
	if resume {
		if ch != nil {
			select {
			case <-ch:
			default:
			}
		}
		m.stateMu.Unlock()
		return true
	}
	m.state = s
	m.lastError = nil
	if s == Ready {
		m.lastReadyTime = time.Now()
	} else if s == Off {
		m.lastOffTime = time.Now()
	}
	close(m.changeCh)
	m.changeCh = make(chan struct{})
	m.stateMu.Unlock()
	return false
}

// cooldownActiveLocked reports whether the cooldown for the given stable state
// is currently active. Caller must hold stateMu.
func (m *Machine) cooldownActiveLocked(state State) bool {
	if m.cooldown <= 0 {
		return false
	}
	var lastTime time.Time
	switch state {
	case Off:
		lastTime = m.lastOffTime
	case Ready:
		lastTime = m.lastReadyTime
	default:
		return false
	}
	if lastTime.IsZero() {
		return false
	}
	return time.Since(lastTime) < m.cooldown
}

// cooldownRemainingLocked returns the remaining cooldown for the given stable
// state, or 0 if none. Caller must hold stateMu.
func (m *Machine) cooldownRemainingLocked(state State) time.Duration {
	if m.cooldown <= 0 {
		return 0
	}
	var lastTime time.Time
	switch state {
	case Off:
		lastTime = m.lastOffTime
	case Ready:
		lastTime = m.lastReadyTime
	default:
		return 0
	}
	if lastTime.IsZero() {
		return 0
	}
	remaining := m.cooldown - time.Since(lastTime)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// State returns the current state of the machine.
func (m *Machine) State() State {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.state
}

func (m *Machine) offCooldownRemaining() time.Duration {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.cooldownRemainingLocked(Off)
}

// EnsureReady blocks until the backend is Ready, or an error/context cancellation occurs.
// Returns nil when Ready, ErrBackendError wrapped with lastError if in Error state,
// context.Canceled/DeadlineExceeded on client timeout/cancellation.
func (m *Machine) EnsureReady(ctx context.Context) error {
	for {
		m.stateMu.Lock()
		current := m.state
		lastErr := m.lastError
		ch := m.changeCh
		m.stateMu.Unlock()

		switch current {
		case Ready:
			return nil
		case Error:
			return fmt.Errorf("%w: %w", ErrBackendError, lastErr)
		case Off, AwaitingGPUFree:
			// Trigger startup if not already in progress, then wait for the
			// state-change signal we captured before calling PowerOn.
			// If PowerOn reports a conflict (another transition still holds
			// transitionMu after setState(Off)), re-evaluate state instead of
			// waiting on a channel that may never close again.
			result := m.PowerOn()
			switch result {
			case ResultAccepted:
				select {
				case <-ch:
				// State changed; loop to re-evaluate.
				case <-ctx.Done():
					return ctx.Err()
				}
			case ResultCooldown:
				remaining := m.offCooldownRemaining()
				if remaining > 0 {
					timer := time.NewTimer(remaining)
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
						return ctx.Err()
					}
				}
				continue
			case ResultConflict, ResultAlreadyInState:
				continue
			}
		case Starting:
			// Another request already triggered startup; wait for state change.
			select {
			case <-ch:
			case <-ctx.Done():
				return ctx.Err()
			}
		case ShuttingDown:
			// Wait for shutdown to complete (→ Off or resume to Ready), then loop back to PowerOn.
			select {
			case <-ch:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
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
		if err != nil {
			m.logger.Debug("nvidia-smi not ready yet", "error", err)
		}
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
		healthy, _, err := m.health.Check(ctx)
		return healthy, err
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

	// Phase 1: stop llama-swap within shutdownTimeout.
	ctx1, cancel1 := context.WithTimeout(context.Background(), m.shutdownTimeout)
	defer cancel1()

	m.logger.Info("Stopping llama-swap")
	if err := m.docker.Stop(ctx1); err != nil {
		m.setState(Error, fmt.Errorf("docker stop failed: %w", err))
		m.logger.Error("Docker stop failed", "error", err)
		return
	}

	m.logger.Info("Waiting for llama-swap to stop")
	if err := m.poll(ctx1, m.pollInterval, func(ctx context.Context) (bool, error) {
		running, err := m.docker.IsRunning(ctx)
		return !running, err
	}); err != nil {
		m.setState(Error, fmt.Errorf("llama-swap stop timeout: %w", err))
		m.logger.Error("llama-swap stop timeout", "error", err)
		return
	}

	// Phase 1b: stop all aux containers with a fresh shutdown timeout.
	if m.aux != nil {
		ctx1b, cancel1b := context.WithTimeout(context.Background(), m.shutdownTimeout)
		defer cancel1b()

		m.logger.Info("Stopping aux containers")
		if err := m.aux.StopAll(ctx1b); err != nil {
			m.setState(Error, fmt.Errorf("aux container stop failed: %w", err))
			m.logger.Error("Aux container stop failed", "error", err)
			return
		}
	}

	// Phase 2: wait indefinitely for GPU to be free of compute processes.
	m.stateMu.Lock()
	m.resumeStartup = false
	m.resumeCh = make(chan struct{}, 1)
	m.stateMu.Unlock()
	m.setState(AwaitingGPUFree, nil)
	m.logger.Info("State -> AwaitingGPUFree")

	free, err := m.awaitGpuFree()
	if err != nil {
		m.setState(Error, fmt.Errorf("gpu process check failed: %w", err))
		m.logger.Error("GPU process check failed", "error", err)
		return
	}
	if !free {
		// Resume signal received; run startup while still holding transitionMu.
		m.startup()
		return
	}

	// Phase 3: unbind and power off within a fresh shutdownTimeout.
	// Atomically check for a resume request that arrived after the GPU became
	// free but before the state transition; if present, run startup instead.
	if m.setStateOrResume(ShuttingDown) {
		m.startup()
		return
	}
	m.logger.Info("State -> ShuttingDown")

	ctx2, cancel2 := context.WithTimeout(context.Background(), m.shutdownTimeout)
	defer cancel2()

	m.logger.Info("Unbinding eGPU drivers")
	if err := m.unbinder.Unbind(ctx2); err != nil {
		m.setState(Error, fmt.Errorf("egpu unbind failed: %w", err))
		m.logger.Error("eGPU unbind failed", "error", err)
		return
	}

	m.logger.Info("Shelly power OFF")
	if err := m.power.SetPower(ctx2, false); err != nil {
		m.setState(Error, fmt.Errorf("shelly power off failed: %w", err))
		m.logger.Error("Shelly power OFF failed", "error", err)
		return
	}

	m.logger.Info("Waiting for GPU to disappear")
	if err := m.poll(ctx2, m.pollInterval, func(ctx context.Context) (bool, error) {
		present, _, err := m.gpu.Status(ctx)
		if err != nil {
			m.logger.Debug("nvidia-smi failed during shutdown, treating GPU as gone", "error", err)
			return true, nil
		}
		return !present, nil
	}); err != nil {
		m.setState(Error, fmt.Errorf("gpu power off timeout: %w", err))
		m.logger.Error("GPU power off timeout", "error", err)
		return
	}

	m.setState(Off, nil)
	m.logger.Info("State -> Off")
}

func (m *Machine) requestResume() {
	m.stateMu.Lock()
	m.resumeStartup = true
	ch := m.resumeCh
	m.stateMu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// consumeResume checks whether a resume request is pending and drains the
// resume channel if so. It returns true when a startup should be resumed.
// It resets resumeStartup under stateMu and is safe to call from any goroutine.
func (m *Machine) consumeResume() bool {
	m.stateMu.Lock()
	resume := m.resumeStartup
	m.resumeStartup = false
	ch := m.resumeCh
	m.stateMu.Unlock()
	if resume && ch != nil {
		select {
		case <-ch:
		default:
		}
	}
	return resume
}

func (m *Machine) awaitGpuFree() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	procs, err := m.gpu.Processes(ctx)
	if err != nil {
		return false, err
	}
	if len(procs) == 0 {
		if m.consumeResume() {
			return false, nil
		}
		return true, nil
	}

	m.logger.Info("GPU processes detected, waiting for them to clear", "count", len(procs))

	ticker := time.NewTicker(m.gpuFreeCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			procs, err := m.gpu.Processes(ctx)
			cancel()
			if err != nil {
				return false, err
			}
			if len(procs) == 0 {
				if m.consumeResume() {
					return false, nil
				}
				return true, nil
			}
		case <-m.resumeCh:
			if m.consumeResume() {
				return false, nil
			}
			// Spurious wake: continue waiting.
		}
	}
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
	if current == Ready {
		// shutdown() already ran startup() (resume from AwaitingGPUFree).
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
