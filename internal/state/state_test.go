package state

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePower struct {
	mu       sync.Mutex
	on       bool
	setErr   error
	isOnErr  error
	setCalls []bool
	gpu      *fakeGPU
	block    chan struct{}
}

func (f *fakePower) SetPower(ctx context.Context, on bool) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, on)
	if f.setErr != nil {
		return f.setErr
	}
	f.on = on
	if f.gpu != nil {
		f.gpu.present = on
	}
	return nil
}

func (f *fakePower) IsOn(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.isOnErr != nil {
		return false, f.isOnErr
	}
	return f.on, nil
}

type fakeGPU struct {
	mu      sync.Mutex
	present bool
	name    string
	err     error
}

func (f *fakeGPU) Status(ctx context.Context) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.present {
		return true, f.name, nil
	}
	return false, "", f.err
}

type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(ctx context.Context, level slog.Level) bool { return true }

func (h *recordingHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(name string) slog.Handler { return h }

func (h *recordingHandler) hasRecord(level slog.Level, msgSubstr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, msgSubstr) {
			return true
		}
	}
	return false
}

type fakeDocker struct {
	mu           sync.Mutex
	running      bool
	startErr     error
	stopErr      error
	isRunningErr error
	block        chan struct{} // blocks both Start and Stop until closed
}

func (f *fakeDocker) Start(ctx context.Context) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	return nil
}

func (f *fakeDocker) Stop(ctx context.Context) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErr != nil {
		return f.stopErr
	}
	f.running = false
	return nil
}

func (f *fakeDocker) IsRunning(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.isRunningErr != nil {
		return false, f.isRunningErr
	}
	return f.running, nil
}

type fakeHealth struct {
	mu      sync.Mutex
	healthy bool
	models  []string
	err     error
}

func (f *fakeHealth) Check(ctx context.Context) (bool, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, nil, f.err
	}
	return f.healthy, f.models, nil
}

type fakeUnbinder struct {
	mu        sync.Mutex
	unbindErr error
	calls     int
}

func (f *fakeUnbinder) Unbind(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.unbindErr
}

func newTestMachine() (*Machine, *fakePower, *fakeGPU, *fakeDocker, *fakeHealth, *fakeUnbinder) {
	return newTestMachineWithCooldown(0)
}

func newTestMachineWithCooldown(cooldown time.Duration) (*Machine, *fakePower, *fakeGPU, *fakeDocker, *fakeHealth, *fakeUnbinder) {
	power := &fakePower{}
	gpu := &fakeGPU{}
	power.gpu = gpu
	docker := &fakeDocker{}
	health := &fakeHealth{}
	unbinder := &fakeUnbinder{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := New(power, gpu, docker, health, unbinder, logger, 10*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond, cooldown)
	return m, power, gpu, docker, health, unbinder
}

func newTestMachineWithRecorder() (*Machine, *fakePower, *fakeGPU, *fakeDocker, *fakeHealth, *fakeUnbinder, *recordingHandler) {
	power := &fakePower{}
	gpu := &fakeGPU{}
	power.gpu = gpu
	docker := &fakeDocker{}
	health := &fakeHealth{}
	unbinder := &fakeUnbinder{}
	handler := &recordingHandler{}
	logger := slog.New(handler)
	m := New(power, gpu, docker, health, unbinder, logger, 10*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond, 0)
	return m, power, gpu, docker, health, unbinder, handler
}

func TestPowerOnFromOff(t *testing.T) {
	m, _, gpu, docker, health, _ := newTestMachine()
	gpu.present = true
	gpu.name = "NVIDIA GeForce RTX 5060 Ti"
	health.healthy = true

	if got := m.PowerOn(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Ready {
		t.Fatalf("expected Ready, got %v", m.state)
	}
	if m.lastError != nil {
		t.Fatalf("expected no lastError, got %v", m.lastError)
	}
	if !docker.running {
		t.Fatalf("expected docker running")
	}
}

func TestPowerOnAlreadyReady(t *testing.T) {
	m, _, _, _, _, _ := newTestMachine()
	m.state = Ready

	if got := m.PowerOn(); got != ResultAlreadyInState {
		t.Fatalf("expected ResultAlreadyInState, got %v", got)
	}
	if m.state != Ready {
		t.Fatalf("expected Ready, got %v", m.state)
	}
}

func TestPowerOnFromError(t *testing.T) {
	m, _, _, _, _, _ := newTestMachine()
	m.state = Error

	if got := m.PowerOn(); got != ResultConflict {
		t.Fatalf("expected ResultConflict, got %v", got)
	}
}

func TestPowerOffFromReady(t *testing.T) {
	m, power, gpu, docker, _, _ := newTestMachine()
	m.state = Ready
	power.on = true
	gpu.present = true
	docker.running = true

	if got := m.PowerOff(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Off {
		t.Fatalf("expected Off, got %v", m.state)
	}
	if m.lastError != nil {
		t.Fatalf("expected no lastError, got %v", m.lastError)
	}
	if power.on {
		t.Fatalf("expected power off")
	}
}

func TestPowerOffFromError(t *testing.T) {
	m, power, gpu, docker, _, _ := newTestMachine()
	m.state = Error
	m.lastError = errors.New("previous error")
	power.on = true
	gpu.present = true
	docker.running = true

	if got := m.PowerOff(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Off {
		t.Fatalf("expected Off, got %v", m.state)
	}
	if m.lastError != nil {
		t.Fatalf("expected no lastError, got %v", m.lastError)
	}
}

func TestPowerOffAlreadyOff(t *testing.T) {
	m, _, _, _, _, _ := newTestMachine()

	if got := m.PowerOff(); got != ResultAlreadyInState {
		t.Fatalf("expected ResultAlreadyInState, got %v", got)
	}
}

func TestRestartFromOff(t *testing.T) {
	m, _, gpu, docker, health, _ := newTestMachine()
	gpu.present = true
	health.healthy = true

	if got := m.Restart(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Ready {
		t.Fatalf("expected Ready, got %v", m.state)
	}
	if !docker.running {
		t.Fatalf("expected docker running")
	}
}

func TestRestartFromReady(t *testing.T) {
	m, power, gpu, docker, health, _ := newTestMachine()
	m.state = Ready
	power.on = true
	gpu.present = true
	docker.running = true
	health.healthy = true

	if got := m.Restart(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Ready {
		t.Fatalf("expected Ready, got %v", m.state)
	}
	if !docker.running {
		t.Fatalf("expected docker running after restart")
	}
}

func TestRestartFromError(t *testing.T) {
	m, _, _, _, _, _ := newTestMachine()
	m.state = Error

	if got := m.Restart(); got != ResultConflict {
		t.Fatalf("expected ResultConflict, got %v", got)
	}
}

func TestStartupShellyError(t *testing.T) {
	m, power, _, _, _, _ := newTestMachine()
	power.setErr = errors.New("connection refused")

	m.PowerOn()
	m.Wait()

	if m.state != Error {
		t.Fatalf("expected Error, got %v", m.state)
	}
	if m.lastError == nil {
		t.Fatalf("expected lastError")
	}
}

func TestStartupGPUTimeout(t *testing.T) {
	m, power, gpu, _, _, _ := newTestMachine()
	gpu.present = false
	// Prevent SetPower from changing the GPU state.
	power.gpu = nil

	m.PowerOn()
	m.Wait()

	if m.state != Error {
		t.Fatalf("expected Error, got %v", m.state)
	}
	assertLastErrorContains(t, m.lastError, "gpu", "timeout")
	if !power.on {
		t.Fatalf("expected shelly powered on before timeout")
	}
}

func TestStartupDockerError(t *testing.T) {
	m, _, gpu, docker, _, _ := newTestMachine()
	gpu.present = true
	docker.startErr = errors.New("docker error")

	m.PowerOn()
	m.Wait()

	if m.state != Error {
		t.Fatalf("expected Error, got %v", m.state)
	}
	if m.lastError == nil {
		t.Fatalf("expected lastError")
	}
}

func TestStartupHealthTimeout(t *testing.T) {
	m, _, gpu, docker, health, _ := newTestMachine()
	gpu.present = true
	health.healthy = false

	m.PowerOn()
	m.Wait()

	if m.state != Error {
		t.Fatalf("expected Error, got %v", m.state)
	}
	assertLastErrorContains(t, m.lastError, "health", "timeout")
	if !docker.running {
		t.Fatalf("expected docker started before health timeout")
	}
}

func TestShutdownDockerError(t *testing.T) {
	m, _, _, docker, _, _ := newTestMachine()
	m.state = Ready
	docker.running = true
	docker.stopErr = errors.New("docker stop error")

	m.PowerOff()
	m.Wait()

	if m.state != Error {
		t.Fatalf("expected Error, got %v", m.state)
	}
	if m.lastError == nil {
		t.Fatalf("expected lastError")
	}
}

func TestShutdownShellyError(t *testing.T) {
	m, power, gpu, docker, _, _ := newTestMachine()
	m.state = Ready
	power.on = true
	gpu.present = true
	docker.running = true
	power.setErr = errors.New("unreachable")

	m.PowerOff()
	m.Wait()

	if m.state != Error {
		t.Fatalf("expected Error, got %v", m.state)
	}
	if m.lastError == nil {
		t.Fatalf("expected lastError")
	}
}

func TestShutdownGPUTimeout(t *testing.T) {
	m, power, gpu, docker, _, _ := newTestMachine()
	m.state = Ready
	power.on = true
	gpu.present = true
	docker.running = true
	// Prevent GPU from disappearing when power is turned off.
	power.gpu = nil

	m.PowerOff()
	m.Wait()

	if m.state != Error {
		t.Fatalf("expected Error, got %v", m.state)
	}
	assertLastErrorContains(t, m.lastError, "gpu", "timeout")
}

func TestShutdownUnbindSuccess(t *testing.T) {
	m, power, gpu, docker, _, unbinder := newTestMachine()
	m.state = Ready
	power.on = true
	gpu.present = true
	docker.running = true

	if got := m.PowerOff(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Off {
		t.Fatalf("expected Off, got %v", m.state)
	}
	if m.lastError != nil {
		t.Fatalf("expected no lastError, got %v", m.lastError)
	}
	if power.on {
		t.Fatalf("expected power off")
	}
	if unbinder.calls != 1 {
		t.Fatalf("expected unbind called once, got %d", unbinder.calls)
	}
}

func TestShutdownUnbindError(t *testing.T) {
	m, power, gpu, docker, _, unbinder := newTestMachine()
	m.state = Ready
	power.on = true
	gpu.present = true
	docker.running = true
	unbinder.unbindErr = errors.New("sudo failed")

	if got := m.PowerOff(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Error {
		t.Fatalf("expected Error, got %v", m.state)
	}
	assertLastErrorContains(t, m.lastError, "unbind")
	if !power.on {
		t.Fatalf("expected power to remain on when unbind fails")
	}
	if unbinder.calls != 1 {
		t.Fatalf("expected unbind called once, got %d", unbinder.calls)
	}
}

func TestShutdownUnbindNotCalledWhenDockerStopFails(t *testing.T) {
	m, _, _, docker, _, unbinder := newTestMachine()
	m.state = Ready
	docker.running = true
	docker.stopErr = errors.New("docker stop error")

	m.PowerOff()
	m.Wait()

	if m.state != Error {
		t.Fatalf("expected Error, got %v", m.state)
	}
	if unbinder.calls != 0 {
		t.Fatalf("expected unbind not called, got %d", unbinder.calls)
	}
}

func TestRestartCallsUnbind(t *testing.T) {
	m, power, gpu, docker, health, unbinder := newTestMachine()
	m.state = Ready
	power.on = true
	gpu.present = true
	docker.running = true
	health.healthy = true

	if got := m.Restart(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Ready {
		t.Fatalf("expected Ready, got %v", m.state)
	}
	if unbinder.calls != 1 {
		t.Fatalf("expected unbind called once during restart shutdown, got %d", unbinder.calls)
	}
}

func TestShutdownFromErrorCallsUnbind(t *testing.T) {
	m, power, gpu, docker, _, unbinder := newTestMachine()
	m.state = Error
	m.lastError = errors.New("previous error")
	power.on = true
	gpu.present = true
	docker.running = true

	if got := m.PowerOff(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Off {
		t.Fatalf("expected Off, got %v", m.state)
	}
	if unbinder.calls != 1 {
		t.Fatalf("expected unbind called once during error-recovery shutdown, got %d", unbinder.calls)
	}
}

func TestStartupGPUErrorThenPresent(t *testing.T) {
	m, power, gpu, docker, health, _, handler := newTestMachineWithRecorder()
	gpu.err = errors.New("nvidia-smi: command not found")
	// Prevent SetPower from changing the GPU state; we drive it manually.
	power.gpu = nil
	docker.running = false
	health.healthy = true

	// After a few failed polls, the GPU becomes present.
	go func() {
		time.Sleep(50 * time.Millisecond)
		gpu.mu.Lock()
		gpu.present = true
		gpu.name = "NVIDIA GeForce RTX 5060 Ti"
		gpu.err = nil
		gpu.mu.Unlock()
	}()

	if got := m.PowerOn(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Ready {
		t.Fatalf("expected Ready, got %v", m.state)
	}
	if !handler.hasRecord(slog.LevelDebug, "nvidia-smi") {
		t.Fatalf("expected DEBUG log containing 'nvidia-smi'")
	}
	if handler.hasRecord(slog.LevelWarn, "nvidia-smi") {
		t.Fatalf("expected no WARN log containing 'nvidia-smi'")
	}
}

func TestStartupGPUErrorTimeout(t *testing.T) {
	m, power, gpu, _, _, _, handler := newTestMachineWithRecorder()
	gpu.err = errors.New("nvidia-smi: command not found")
	// Prevent SetPower from changing the GPU state.
	power.gpu = nil

	if got := m.PowerOn(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Error {
		t.Fatalf("expected Error, got %v", m.state)
	}
	assertLastErrorContains(t, m.lastError, "gpu", "timeout")
	if !handler.hasRecord(slog.LevelDebug, "nvidia-smi") {
		t.Fatalf("expected DEBUG log containing 'nvidia-smi'")
	}
}

func TestShutdownGPUErrorGone(t *testing.T) {
	m, power, gpu, docker, _, _, handler := newTestMachineWithRecorder()
	m.state = Ready
	power.on = true
	gpu.present = true
	gpu.name = "NVIDIA GeForce RTX 5060 Ti"
	docker.running = true
	// Prevent SetPower from changing the GPU state; we drive it manually.
	power.gpu = nil

	// After power is turned off, nvidia-smi starts failing.
	go func() {
		// Wait until power is turned off.
		for {
			power.mu.Lock()
			on := power.on
			power.mu.Unlock()
			if !on {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		gpu.mu.Lock()
		gpu.present = false
		gpu.err = errors.New("nvidia-smi: command not found")
		gpu.mu.Unlock()
	}()

	if got := m.PowerOff(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.state != Off {
		t.Fatalf("expected Off, got %v", m.state)
	}
	if m.lastError != nil {
		t.Fatalf("expected no lastError, got %v", m.lastError)
	}
	if !handler.hasRecord(slog.LevelDebug, "nvidia-smi") {
		t.Fatalf("expected DEBUG log containing 'nvidia-smi'")
	}
	if handler.hasRecord(slog.LevelWarn, "nvidia-smi") {
		t.Fatalf("expected no WARN log containing 'nvidia-smi'")
	}
}

func TestStatusProbeLogLevels(t *testing.T) {
	type stateCase struct {
		name  string
		state State
		quiet bool
	}

	quietStates := []stateCase{
		{name: "Off", state: Off, quiet: true},
		{name: "Starting", state: Starting, quiet: true},
		{name: "ShuttingDown", state: ShuttingDown, quiet: true},
		{name: "Ready", state: Ready, quiet: false},
		{name: "Error", state: Error, quiet: false},
	}

	loudStates := []stateCase{
		{name: "Off", state: Off, quiet: false},
		{name: "Ready", state: Ready, quiet: false},
	}

	cases := []struct {
		name       string
		setErr     func(*Machine, *fakePower, *fakeGPU, *fakeDocker, *fakeHealth)
		assertOK   func(StatusResponse) bool
		msg        string
		stateCases []stateCase
	}{
		{
			name: "GPU",
			setErr: func(_ *Machine, _ *fakePower, gpu *fakeGPU, _ *fakeDocker, _ *fakeHealth) {
				gpu.err = errors.New("nvidia-smi: command not found")
			},
			assertOK:   func(status StatusResponse) bool { return !status.GPUPresent && status.GPUName == "" },
			msg:        "GPU status probe failed",
			stateCases: quietStates,
		},
		{
			name: "Health",
			setErr: func(_ *Machine, _ *fakePower, _ *fakeGPU, _ *fakeDocker, health *fakeHealth) {
				health.err = errors.New("connection refused")
			},
			assertOK:   func(status StatusResponse) bool { return !status.LlamaSwapHealthy },
			msg:        "Health status probe failed",
			stateCases: quietStates,
		},
		{
			name: "Shelly",
			setErr: func(_ *Machine, power *fakePower, _ *fakeGPU, _ *fakeDocker, _ *fakeHealth) {
				power.isOnErr = errors.New("unreachable")
			},
			assertOK:   func(status StatusResponse) bool { return true },
			msg:        "Shelly status probe failed",
			stateCases: loudStates,
		},
		{
			name: "Docker",
			setErr: func(_ *Machine, _ *fakePower, _ *fakeGPU, docker *fakeDocker, _ *fakeHealth) {
				docker.isRunningErr = errors.New("docker daemon unreachable")
			},
			assertOK:   func(status StatusResponse) bool { return true },
			msg:        "Docker status probe failed",
			stateCases: loudStates,
		},
	}

	for _, probe := range cases {
		for _, st := range probe.stateCases {
			t.Run(probe.name+"/"+st.name, func(t *testing.T) {
				m, power, gpu, docker, health, _, handler := newTestMachineWithRecorder()
				probe.setErr(m, power, gpu, docker, health)
				m.stateMu.Lock()
				m.state = st.state
				m.stateMu.Unlock()

				status := m.Status()

				if !probe.assertOK(status) {
					t.Fatalf("unexpected status response: %+v", status)
				}
				if got := handler.hasRecord(slog.LevelDebug, probe.msg); got != st.quiet {
					t.Fatalf("expected DEBUG log present=%v, got %v", st.quiet, got)
				}
				if got := handler.hasRecord(slog.LevelWarn, probe.msg); got == st.quiet {
					t.Fatalf("expected WARN log present=%v, got %v", !st.quiet, got)
				}
			})
		}
	}
}

func TestConcurrentTransitions(t *testing.T) {
	m, power, _, _, _, _ := newTestMachine()
	// Block SetPower so the startup transition stays in progress.
	power.block = make(chan struct{})

	m.PowerOn()

	if got := m.PowerOn(); got != ResultConflict {
		t.Fatalf("expected concurrent PowerOn to return ResultConflict, got %v", got)
	}
	if got := m.PowerOff(); got != ResultConflict {
		t.Fatalf("expected concurrent PowerOff to return ResultConflict, got %v", got)
	}
	if got := m.Restart(); got != ResultConflict {
		t.Fatalf("expected concurrent Restart to return ResultConflict, got %v", got)
	}

	close(power.block)
	m.Wait()
}

func TestShutdownConcurrentTransitions(t *testing.T) {
	m, power, gpu, docker, _, _ := newTestMachine()
	m.state = Ready
	power.on = true
	gpu.present = true
	docker.running = true
	// Block SetPower so the shutdown transition stays in progress.
	power.block = make(chan struct{})

	m.PowerOff()

	if got := m.PowerOn(); got != ResultConflict {
		t.Fatalf("expected concurrent PowerOn to return ResultConflict, got %v", got)
	}
	if got := m.PowerOff(); got != ResultConflict {
		t.Fatalf("expected concurrent PowerOff to return ResultConflict, got %v", got)
	}
	if got := m.Restart(); got != ResultConflict {
		t.Fatalf("expected concurrent Restart to return ResultConflict, got %v", got)
	}

	close(power.block)
	m.Wait()
}

func TestStatus(t *testing.T) {
	cases := []struct {
		name             string
		state            State
		lastError        error
		gpuPresent       bool
		gpuName          string
		shellyOn         bool
		dockerRunning    bool
		healthHealthy    bool
		healthModels     []string
		wantState        string
		wantLastErrorNil bool
		wantLoadedModels []string
	}{
		{
			name:             "ready status",
			state:            Ready,
			gpuPresent:       true,
			gpuName:          "NVIDIA GeForce RTX 5060 Ti",
			shellyOn:         true,
			dockerRunning:    true,
			healthHealthy:    true,
			wantState:        "Ready",
			wantLastErrorNil: true,
			wantLoadedModels: []string{},
		},
		{
			name:             "error status",
			state:            Error,
			lastError:        errors.New("something went wrong"),
			wantState:        "Error",
			wantLastErrorNil: false,
			wantLoadedModels: []string{},
		},
		{
			name:             "off status",
			state:            Off,
			wantState:        "Off",
			wantLastErrorNil: true,
			wantLoadedModels: []string{},
		},
		{
			name:             "ready with loaded models",
			state:            Ready,
			gpuPresent:       true,
			gpuName:          "NVIDIA GeForce RTX 5060 Ti",
			shellyOn:         true,
			dockerRunning:    true,
			healthHealthy:    true,
			healthModels:     []string{"qwen3.5-9b"},
			wantState:        "Ready",
			wantLastErrorNil: true,
			wantLoadedModels: []string{"qwen3.5-9b"},
		},
		{
			name:             "ready unhealthy",
			state:            Ready,
			gpuPresent:       true,
			gpuName:          "NVIDIA GeForce RTX 5060 Ti",
			shellyOn:         true,
			dockerRunning:    true,
			healthHealthy:    false,
			wantState:        "Ready",
			wantLastErrorNil: true,
			wantLoadedModels: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, power, gpu, docker, health, _ := newTestMachine()
			m.state = tc.state
			m.lastError = tc.lastError
			gpu.present = tc.gpuPresent
			gpu.name = tc.gpuName
			power.on = tc.shellyOn
			docker.running = tc.dockerRunning
			health.healthy = tc.healthHealthy
			health.models = tc.healthModels

			status := m.Status()
			if status.State != tc.wantState {
				t.Fatalf("expected state %q, got %q", tc.wantState, status.State)
			}
			if status.GPUPresent != tc.gpuPresent {
				t.Fatalf("expected gpuPresent %v, got %v", tc.gpuPresent, status.GPUPresent)
			}
			if status.GPUName != tc.gpuName {
				t.Fatalf("expected gpuName %q, got %q", tc.gpuName, status.GPUName)
			}
			if status.ShellyOn != tc.shellyOn {
				t.Fatalf("expected shellyOn %v, got %v", tc.shellyOn, status.ShellyOn)
			}
			if status.LlamaSwapRunning != tc.dockerRunning {
				t.Fatalf("expected llamaSwapRunning %v, got %v", tc.dockerRunning, status.LlamaSwapRunning)
			}
			if status.LlamaSwapHealthy != tc.healthHealthy {
				t.Fatalf("expected llamaSwapHealthy %v, got %v", tc.healthHealthy, status.LlamaSwapHealthy)
			}
			if tc.wantLastErrorNil && status.LastError != nil {
				t.Fatalf("expected lastError nil, got %v", *status.LastError)
			}
			if !tc.wantLastErrorNil && status.LastError == nil {
				t.Fatalf("expected lastError non-nil")
			}
			if !reflect.DeepEqual(status.LoadedModels, tc.wantLoadedModels) {
				t.Fatalf("expected loadedModels %v, got %v", tc.wantLoadedModels, status.LoadedModels)
			}
		})
	}
}

func TestStatusResponseJSON(t *testing.T) {
	m, _, _, _, health, _ := newTestMachine()
	m.state = Ready
	health.healthy = true

	status := m.Status()
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	if !strings.Contains(string(data), `"loadedModels":[]`) {
		t.Fatalf("expected JSON to contain \"loadedModels\":[], got %s", string(data))
	}
}

func assertLastErrorContains(t *testing.T, err error, substrs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected lastError")
	}
	msg := err.Error()
	for _, s := range substrs {
		if !strings.Contains(strings.ToLower(msg), strings.ToLower(s)) {
			t.Fatalf("expected lastError %q to contain %q", msg, s)
		}
	}
}

func TestState(t *testing.T) {
	m, _, _, _, _, _ := newTestMachine()

	cases := []struct {
		state State
		want  string
	}{
		{Off, "Off"},
		{Starting, "Starting"},
		{Ready, "Ready"},
		{ShuttingDown, "ShuttingDown"},
		{Error, "Error"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			m.stateMu.Lock()
			m.state = tc.state
			m.stateMu.Unlock()

			got := m.State()
			if got != tc.state {
				t.Errorf("expected State %s, got %s", tc.want, got.String())
			}
		})
	}
}

func TestEnsureReady_WhenReady(t *testing.T) {
	m, _, gpu, _, health, _ := newTestMachine()
	gpu.present = true
	health.healthy = true

	// Power on first so state becomes Ready
	if got := m.PowerOn(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	ctx := context.Background()
	err := m.EnsureReady(ctx)
	if err != nil {
		t.Fatalf("EnsureReady on Ready state returned error: %v", err)
	}
}

func TestEnsureReady_WhenError(t *testing.T) {
	m, _, _, _, _, _ := newTestMachine()

	// Set state to Error with a lastError
	lastErr := errors.New("gpu timeout")
	m.stateMu.Lock()
	m.state = Error
	m.lastError = lastErr
	m.stateMu.Unlock()

	err := m.EnsureReady(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrBackendError) {
		t.Fatalf("expected error wrapping ErrBackendError, got: %v", err)
	}
	if !strings.Contains(err.Error(), "gpu timeout") {
		t.Errorf("expected error to contain original lastError, got: %v", err)
	}
}

func TestEnsureReady_WhenOff(t *testing.T) {
	m, _, gpu, _, health, _ := newTestMachine()
	gpu.present = true
	health.healthy = true

	ctx := context.Background()
	err := m.EnsureReady(ctx)
	if err != nil {
		t.Fatalf("EnsureReady returned error: %v", err)
	}
	m.Wait()

	if m.State() != Ready {
		t.Errorf("expected state to be Ready after EnsureReady, got %s", m.State())
	}
}

func TestEnsureReady_WhenStarting(t *testing.T) {
	m, _, gpu, docker, health, _ := newTestMachine()
	gpu.present = true
	health.healthy = true

	// Block docker.Start so the startup transition stays in Starting.
	docker.block = make(chan struct{})
	if got := m.PowerOn(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}

	// Wait for state to actually become Starting.
	for i := 0; i < 100; i++ {
		if m.State() == Starting {
			break
		}
		time.Sleep(1 * time.Millisecond)
	}
	if m.State() != Starting {
		t.Fatalf("expected state to become Starting, got %s", m.State())
	}

	// EnsureReady should wait for the startup to complete.
	done := make(chan error, 1)
	go func() {
		done <- m.EnsureReady(context.Background())
	}()

	// Unblock startup and let it complete.
	close(docker.block)

	err := <-done
	if err != nil {
		t.Fatalf("EnsureReady returned error: %v", err)
	}

	m.Wait()
	if m.State() != Ready {
		t.Errorf("expected state to be Ready, got %s", m.State())
	}
}

func TestEnsureReady_WhenShuttingDown(t *testing.T) {
	m, _, gpu, docker, health, _ := newTestMachine()
	gpu.present = true
	health.healthy = true

	// Power on first to get to Ready
	if got := m.PowerOn(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	// Block docker.Stop so shutdown stalls after setState(ShuttingDown)
	docker.block = make(chan struct{})
	if got := m.PowerOff(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}

	// Wait for state to actually become ShuttingDown (goroutine has started)
	for i := 0; i < 100; i++ {
		if m.State() == ShuttingDown {
			break
		}
		time.Sleep(1 * time.Millisecond)
	}
	if m.State() != ShuttingDown {
		t.Fatalf("expected state to become ShuttingDown, got %s", m.State())
	}

	// EnsureReady should wait for shutdown to complete, then trigger PowerOn
	done := make(chan error, 1)
	go func() {
		done <- m.EnsureReady(context.Background())
	}()

	// Let shutdown proceed by unblocking docker.Stop
	close(docker.block)

	err := <-done
	if err != nil {
		t.Fatalf("EnsureReady returned error: %v", err)
	}

	m.Wait()
	if m.State() != Ready {
		t.Errorf("expected state to be Ready after EnsureReady from ShuttingDown, got %s", m.State())
	}
}

func TestEnsureReady_ContextCanceled(t *testing.T) {
	m, _, _, _, _, _ := newTestMachine()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := m.EnsureReady(ctx)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if m.State() == Error {
		t.Error("expected state machine not in Error state after context cancellation")
	}
}

func TestEnsureReady_ContextDeadlineExceeded(t *testing.T) {
	m, _, gpu, docker, health, _ := newTestMachine()
	gpu.present = true
	health.healthy = true

	// Block docker.Start so startup takes longer than our context deadline
	docker.block = make(chan struct{})

	// Use a very short timeout — startup will be blocked by docker.block
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	err := m.EnsureReady(ctx)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}

	// Unblock and let startup complete so state machine is clean for subsequent tests
	close(docker.block)
	m.Wait()
}

func TestEnsureReady_ConcurrentCalls(t *testing.T) {
	m, _, gpu, _, health, _ := newTestMachine()
	gpu.present = true
	health.healthy = true

	const N = 5
	var wg sync.WaitGroup
	errCh := make(chan error, N)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errCh <- m.EnsureReady(ctx)
		}()
	}

	wg.Wait()
	close(errCh)

	// All N calls should return nil when startup succeeds
	for err := range errCh {
		if err != nil {
			t.Errorf("EnsureReady returned error: %v", err)
		}
	}

	m.Wait()
	if m.State() != Ready {
		t.Errorf("expected state to be Ready, got %s", m.State())
	}

	// Verify only one startup was actually triggered (power.setCalls should have exactly one true)
	power := m.power.(*fakePower)
	power.mu.Lock()
	setTrueCount := 0
	for _, on := range power.setCalls {
		if on {
			setTrueCount++
		}
	}
	power.mu.Unlock()

	// Should be exactly 1: one PowerOn → SetPower(true) call
	// (plus any shutdown calls if EnsureReady went through ShuttingDown first, but it shouldn't since we started from Off)
	if setTrueCount != 1 {
		t.Errorf("expected exactly 1 power-on call, got %d", setTrueCount)
	}
}

func TestCooldown(t *testing.T) {
	cases := []struct {
		name         string
		cooldown     time.Duration
		state        State
		setOffTime   bool
		offTime      time.Time
		setReadyTime bool
		readyTime    time.Time
		action       string
		want         PowerResult
		wantState    State
	}{
		{
			name:      "disabled cooldown allows PowerOn from Off",
			cooldown:  0,
			state:     Off,
			action:    "PowerOn",
			want:      ResultAccepted,
			wantState: Off,
		},
		{
			name:      "disabled cooldown allows PowerOff from Ready",
			cooldown:  0,
			state:     Ready,
			action:    "PowerOff",
			want:      ResultAccepted,
			wantState: Ready,
		},
		{
			name:       "post-shutdown cooldown blocks PowerOn",
			cooldown:   50 * time.Millisecond,
			state:      Off,
			setOffTime: true,
			offTime:    time.Now(),
			action:     "PowerOn",
			want:       ResultCooldown,
			wantState:  Off,
		},
		{
			name:       "expired post-shutdown cooldown allows PowerOn",
			cooldown:   50 * time.Millisecond,
			state:      Off,
			setOffTime: true,
			offTime:    time.Now().Add(-51 * time.Millisecond),
			action:     "PowerOn",
			want:       ResultAccepted,
			wantState:  Off,
		},
		{
			name:         "post-startup cooldown blocks PowerOff",
			cooldown:     50 * time.Millisecond,
			state:        Ready,
			setReadyTime: true,
			readyTime:    time.Now(),
			action:       "PowerOff",
			want:         ResultCooldown,
			wantState:    Ready,
		},
		{
			name:         "expired post-startup cooldown allows PowerOff",
			cooldown:     50 * time.Millisecond,
			state:        Ready,
			setReadyTime: true,
			readyTime:    time.Now().Add(-51 * time.Millisecond),
			action:       "PowerOff",
			want:         ResultAccepted,
			wantState:    Ready,
		},
		{
			name:      "first startup not blocked when lastOffTime is zero",
			cooldown:  50 * time.Millisecond,
			state:     Off,
			action:    "PowerOn",
			want:      ResultAccepted,
			wantState: Off,
		},
		{
			name:         "PowerOff not blocked when lastReadyTime is zero",
			cooldown:     50 * time.Millisecond,
			state:        Ready,
			setReadyTime: false,
			action:       "PowerOff",
			want:         ResultAccepted,
			wantState:    Ready,
		},
		{
			name:         "Error recovery exempt from post-startup cooldown",
			cooldown:     50 * time.Millisecond,
			state:        Error,
			setReadyTime: true,
			readyTime:    time.Now(),
			action:       "PowerOff",
			want:         ResultAccepted,
			wantState:    Error,
		},
		{
			name:      "PowerOn from Error always conflict",
			cooldown:  50 * time.Millisecond,
			state:     Error,
			action:    "PowerOn",
			want:      ResultConflict,
			wantState: Error,
		},
		{
			name:       "post-shutdown cooldown blocks Restart from Off",
			cooldown:   50 * time.Millisecond,
			state:      Off,
			setOffTime: true,
			offTime:    time.Now(),
			action:     "Restart",
			want:       ResultCooldown,
			wantState:  Off,
		},
		{
			name:         "post-startup cooldown blocks Restart from Ready",
			cooldown:     50 * time.Millisecond,
			state:        Ready,
			setReadyTime: true,
			readyTime:    time.Now(),
			action:       "Restart",
			want:         ResultCooldown,
			wantState:    Ready,
		},
		{
			name:      "transition in progress takes precedence over cooldown PowerOn",
			cooldown:  50 * time.Millisecond,
			state:     Starting,
			action:    "PowerOn",
			want:      ResultConflict,
			wantState: Starting,
		},
		{
			name:      "transition in progress takes precedence over cooldown PowerOff",
			cooldown:  50 * time.Millisecond,
			state:     Starting,
			action:    "PowerOff",
			want:      ResultConflict,
			wantState: Starting,
		},
		{
			name:       "already Off returns AlreadyInState despite cooldown",
			cooldown:   50 * time.Millisecond,
			state:      Off,
			setOffTime: true,
			offTime:    time.Now(),
			action:     "PowerOff",
			want:       ResultAlreadyInState,
			wantState:  Off,
		},
		{
			name:         "already Ready returns AlreadyInState despite cooldown",
			cooldown:     50 * time.Millisecond,
			state:        Ready,
			setReadyTime: true,
			readyTime:    time.Now(),
			action:       "PowerOn",
			want:         ResultAlreadyInState,
			wantState:    Ready,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, gpu, docker, health, _ := newTestMachineWithCooldown(tc.cooldown)
			m.stateMu.Lock()
			m.state = tc.state
			if tc.setOffTime {
				m.lastOffTime = tc.offTime
			}
			if tc.setReadyTime {
				m.lastReadyTime = tc.readyTime
			}
			m.stateMu.Unlock()
			gpu.present = true
			health.healthy = true
			if tc.state == Ready {
				docker.running = true
			}

			var got PowerResult
			switch tc.action {
			case "PowerOn":
				got = m.PowerOn()
			case "PowerOff":
				got = m.PowerOff()
			case "Restart":
				got = m.Restart()
			default:
				t.Fatalf("unknown action %q", tc.action)
			}

			if got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
			if got == ResultAccepted {
				m.Wait()
				var wantFinal State
				switch tc.action {
				case "PowerOn", "Restart":
					wantFinal = Ready
				case "PowerOff":
					wantFinal = Off
				}
				if m.State() != wantFinal {
					t.Fatalf("expected final state %v, got %v", wantFinal, m.State())
				}
			} else {
				if m.State() != tc.wantState {
					t.Fatalf("expected state %v, got %v", tc.wantState, m.State())
				}
			}
		})
	}
}

func TestCooldown_RealStartupBlocksShutdown(t *testing.T) {
	m, _, gpu, _, health, _ := newTestMachineWithCooldown(50 * time.Millisecond)
	gpu.present = true
	health.healthy = true

	if got := m.PowerOn(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted, got %v", got)
	}
	m.Wait()

	if m.State() != Ready {
		t.Fatalf("expected Ready, got %v", m.State())
	}

	// Immediate PowerOff should be blocked by post-startup cooldown.
	if got := m.PowerOff(); got != ResultCooldown {
		t.Fatalf("expected ResultCooldown, got %v", got)
	}
	if m.State() != Ready {
		t.Fatalf("expected state Ready, got %v", m.State())
	}

	// Wait for cooldown to expire.
	time.Sleep(60 * time.Millisecond)
	if got := m.PowerOff(); got != ResultAccepted {
		t.Fatalf("expected ResultAccepted after cooldown, got %v", got)
	}
	m.Wait()

	if m.State() != Off {
		t.Fatalf("expected Off, got %v", m.State())
	}

	// Immediate PowerOn should be blocked by post-shutdown cooldown.
	if got := m.PowerOn(); got != ResultCooldown {
		t.Fatalf("expected ResultCooldown, got %v", got)
	}
	if m.State() != Off {
		t.Fatalf("expected state Off, got %v", m.State())
	}
}

func TestCooldown_Status(t *testing.T) {
	cases := []struct {
		name              string
		cooldown          time.Duration
		state             State
		setOffTime        bool
		setReadyTime      bool
		wantRemainingZero bool
	}{
		{
			name:              "Off with active cooldown shows remaining",
			cooldown:          50 * time.Millisecond,
			state:             Off,
			setOffTime:        true,
			wantRemainingZero: false,
		},
		{
			name:              "Off with expired cooldown shows zero",
			cooldown:          50 * time.Millisecond,
			state:             Off,
			setOffTime:        true,
			wantRemainingZero: true,
		},
		{
			name:              "Ready with active cooldown shows remaining",
			cooldown:          50 * time.Millisecond,
			state:             Ready,
			setReadyTime:      true,
			wantRemainingZero: false,
		},
		{
			name:              "Starting shows zero cooldown",
			cooldown:          50 * time.Millisecond,
			state:             Starting,
			wantRemainingZero: true,
		},
		{
			name:              "Error shows zero cooldown",
			cooldown:          50 * time.Millisecond,
			state:             Error,
			wantRemainingZero: true,
		},
		{
			name:              "disabled cooldown shows zero",
			cooldown:          0,
			state:             Off,
			setOffTime:        true,
			wantRemainingZero: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _, _, _, _ := newTestMachineWithCooldown(tc.cooldown)
			m.stateMu.Lock()
			m.state = tc.state
			if tc.setOffTime {
				if tc.wantRemainingZero {
					m.lastOffTime = time.Now().Add(-51 * time.Millisecond)
				} else {
					m.lastOffTime = time.Now()
				}
			}
			if tc.setReadyTime {
				if tc.wantRemainingZero {
					m.lastReadyTime = time.Now().Add(-51 * time.Millisecond)
				} else {
					m.lastReadyTime = time.Now()
				}
			}
			m.stateMu.Unlock()

			status := m.Status()
			if tc.wantRemainingZero {
				if status.CooldownRemaining != 0 {
					t.Fatalf("expected cooldownRemaining 0, got %v", status.CooldownRemaining)
				}
			} else {
				if status.CooldownRemaining <= 0 {
					t.Fatalf("expected positive cooldownRemaining, got %v", status.CooldownRemaining)
				}
			}
		})
	}
}

func TestCooldown_EnsureReadyWaits(t *testing.T) {
	m, _, gpu, _, health, _ := newTestMachineWithCooldown(50 * time.Millisecond)
	gpu.present = true
	health.healthy = true

	// Set lastOffTime to now so PowerOn returns ResultCooldown.
	m.stateMu.Lock()
	m.lastOffTime = time.Now()
	m.stateMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := m.EnsureReady(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if m.State() != Ready {
		t.Fatalf("expected Ready, got %v", m.State())
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("EnsureReady returned too quickly (%v), expected to wait for cooldown", elapsed)
	}
}

func TestCooldown_EnsureReadyDeadlineExceeded(t *testing.T) {
	m, _, gpu, _, health, _ := newTestMachineWithCooldown(50 * time.Millisecond)
	gpu.present = true
	health.healthy = true

	m.stateMu.Lock()
	m.lastOffTime = time.Now()
	m.stateMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := m.EnsureReady(ctx)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestCooldown_EnsureReadyCanceled(t *testing.T) {
	m, _, _, _, _, _ := newTestMachineWithCooldown(50 * time.Millisecond)

	m.stateMu.Lock()
	m.lastOffTime = time.Now()
	m.stateMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.EnsureReady(ctx)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
