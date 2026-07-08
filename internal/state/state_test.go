package state

import (
	"context"
	"errors"
	"log/slog"
	"os"
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
}

func (f *fakeGPU) Status(ctx context.Context) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.present, f.name, nil
}

type fakeDocker struct {
	mu       sync.Mutex
	running  bool
	startErr error
	stopErr  error
}

func (f *fakeDocker) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	return nil
}

func (f *fakeDocker) Stop(ctx context.Context) error {
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
	return f.running, nil
}

type fakeHealth struct {
	mu      sync.Mutex
	healthy bool
}

func (f *fakeHealth) Check(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthy, nil
}

func newTestMachine() (*Machine, *fakePower, *fakeGPU, *fakeDocker, *fakeHealth) {
	power := &fakePower{}
	gpu := &fakeGPU{}
	power.gpu = gpu
	docker := &fakeDocker{}
	health := &fakeHealth{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := New(power, gpu, docker, health, logger, 10*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond)
	return m, power, gpu, docker, health
}

func TestPowerOnFromOff(t *testing.T) {
	m, _, gpu, docker, health := newTestMachine()
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
	m, _, _, _, _ := newTestMachine()
	m.state = Ready

	if got := m.PowerOn(); got != ResultAlreadyInState {
		t.Fatalf("expected ResultAlreadyInState, got %v", got)
	}
	if m.state != Ready {
		t.Fatalf("expected Ready, got %v", m.state)
	}
}

func TestPowerOnFromError(t *testing.T) {
	m, _, _, _, _ := newTestMachine()
	m.state = Error

	if got := m.PowerOn(); got != ResultConflict {
		t.Fatalf("expected ResultConflict, got %v", got)
	}
}

func TestPowerOffFromReady(t *testing.T) {
	m, power, gpu, docker, _ := newTestMachine()
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
	m, power, gpu, docker, _ := newTestMachine()
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
	m, _, _, _, _ := newTestMachine()

	if got := m.PowerOff(); got != ResultAlreadyInState {
		t.Fatalf("expected ResultAlreadyInState, got %v", got)
	}
}

func TestRestartFromOff(t *testing.T) {
	m, _, gpu, docker, health := newTestMachine()
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
	m, power, gpu, docker, health := newTestMachine()
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
	m, _, _, _, _ := newTestMachine()
	m.state = Error

	if got := m.Restart(); got != ResultConflict {
		t.Fatalf("expected ResultConflict, got %v", got)
	}
}

func TestStartupShellyError(t *testing.T) {
	m, power, _, _, _ := newTestMachine()
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
	m, power, gpu, _, _ := newTestMachine()
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
	m, _, gpu, docker, _ := newTestMachine()
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
	m, _, gpu, docker, health := newTestMachine()
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
	m, _, _, docker, _ := newTestMachine()
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
	m, power, gpu, docker, _ := newTestMachine()
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
	m, power, gpu, docker, _ := newTestMachine()
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

func TestConcurrentTransitions(t *testing.T) {
	m, power, _, _, _ := newTestMachine()
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
	m, power, gpu, docker, _ := newTestMachine()
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
		wantState        string
		wantLastErrorNil bool
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
		},
		{
			name:             "error status",
			state:            Error,
			lastError:        errors.New("something went wrong"),
			wantState:        "Error",
			wantLastErrorNil: false,
		},
		{
			name:             "off status",
			state:            Off,
			wantState:        "Off",
			wantLastErrorNil: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, power, gpu, docker, health := newTestMachine()
			m.state = tc.state
			m.lastError = tc.lastError
			gpu.present = tc.gpuPresent
			gpu.name = tc.gpuName
			power.on = tc.shellyOn
			docker.running = tc.dockerRunning
			health.healthy = tc.healthHealthy

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
		})
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
