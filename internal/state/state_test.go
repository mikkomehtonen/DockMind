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
	block    chan struct{} // blocks both Start and Stop until closed
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

func TestState(t *testing.T) {
	m, _, _, _, _ := newTestMachine()

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
	m, _, gpu, _, health := newTestMachine()
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
	m, _, _, _, _ := newTestMachine()

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
	m, _, gpu, _, health := newTestMachine()
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
	m, _, gpu, docker, health := newTestMachine()
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
	m, _, gpu, docker, health := newTestMachine()
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
	m, _, _, _, _ := newTestMachine()

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
	m, _, gpu, docker, health := newTestMachine()
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
	m, _, gpu, _, health := newTestMachine()
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
