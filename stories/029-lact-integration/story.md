# Integrate LACT daemon lifecycle into GPU power transitions

## Context

When `lactd` (LACT systemd service) is running on the host while DockMind unbinds an eGPU, lactd loses its ability to detect the GPU permanently until manually restarted. The current manual workaround requires four steps: stop lactd → power off → power on → start lactd. Integrating these two systemctl commands into DockMind's existing shutdown/startup transitions eliminates this fragile manual process and prevents lactd from breaking during every power cycle.

## Out of Scope

- LACT installation or configuration (assumed pre-installed as systemd service)
- Any runtime integration with LACT APIs beyond stopping/starting the `lactd` service
- Polling lactd status for `/status` endpoint
- Handling other GPU management tools

## Implementation approach

Create a new package `internal/lact` following the established pattern from `internal/unbind`: injectable `execFunc`, default `exec.CommandContext`, returns `([]byte, error)`. The client provides two methods: `Stop(ctx)` and `Start(ctx)` that execute `sudo -n /usr/bin/systemctl stop lactd` and `sudo -n /usr/bin/systemctl start lactd` respectively.

The feature is gated behind a new config block `[lact]` with an `enabled` boolean field. When disabled, the state machine skips LACT calls entirely (no-op). The Machine struct holds a pointer to `*lact.Client`, set via `SetLactClient()` if configured — nil means feature inactive.

Shutdown hook: Call `m.lact.Stop(ctx)` immediately after Phase 1b stops auxiliary containers and before entering the GPU-free polling loop / unbind call. This mirrors the existing aux container stop placement in state.go.

Startup hook: Call `m.lact.Start(ctx)` after the GPU detection poll loop confirms the GPU is present (logs "GPU detected") and before llama-swap container start. This ensures lactd restarts only when the GPU driver is ready, avoiding spurious failures if the GPU hasn't appeared yet.

Error handling follows the aux manager pattern: non-fatal errors are logged with `slog.Warn` but do not abort the transition. The shutdown or startup continues regardless of LACT daemon state.

## Tasks

### Task 1 — Create lact package

- Package `internal/lact` defines Client struct with injectable execFunc
  - → New() constructor defaults to exec.CommandContext
  - → SetExec(execFunc) replaces exec for all internal use (test support)
- Stop(ctx) executes "sudo" "-n" "/usr/bin/systemctl" "stop" "lactd"
  - → returns nil on success
  - → returns error from exec on failure
- Start(ctx) executes "sudo" "-n" "/usr/bin/systemctl" "start" "lactd"
  - → returns nil on success
  - → returns error from exec on failure
- Table-driven tests inject fake execFunc to verify correct command invocation
  - → Stop passes correct args ["stop", "lactd"] to systemctl
  - → Start passes correct args ["start", "lactd"] to systemctl

### Task 2 — Add config field

- Config struct gains `Lact struct { Enabled bool } yaml:"lact"` field on AppConfig
  - → YAML `[lact]: enabled: true` unmarshals correctly
  - → Missing section defaults to zero value (Enabled = false)
- Unit test for config.Load with lact block present and absent
  - → Present `enabled: true` produces cfg.Lact.Enabled == true
  - → Absent section produces cfg.Lact.Enabled == false

### Task 3 — Wire in main.go

- When cfg.Lact.Enabled is true, create `lact.New()` client and call `machine.SetLactClient(lactClient)`
  - → machine has non-nil lact field when enabled
  - → machine has nil lact field when disabled
- Existing behavior unchanged when lact section is absent or disabled

### Task 4 — Hook into shutdown transition

- In state.go shutdown() method, after Phase 1b (aux containers stopped), call `m.lact.Stop(ctx)` if m.lact != nil
  - → Stop called before unbind service activation / GPU-free polling
  - → Error logged at Warn level with "lact stop failed" message
- Shutdown continues to next phase regardless of Stop() error
  - → Power still cuts after unbind succeeds
  - → State machine reaches Off state even if lact stop fails

### Task 5 — Hook into startup transition

- In state.go startup() method, after GPU detection poll loop completes (logs "GPU detected"), call `m.lact.Start(ctx)` if m.lact != nil
  - → Start called before llama-swap container start
  - → Error logged at Warn level with "lact start failed" message
- Startup continues to next phase regardless of Start() error
  - → Llama-swap container still starts
  - → State machine reaches On state even if lact start fails

### Task 6 — Update config documentation

- docs/config.yaml.example (if it exists) adds `[lact]` section with `enabled: false` default
- Any other config reference docs updated to mention the new section

## Technical Context

- Go 1.24.4, module `github.com/dockmind/dockmind`. No new external dependencies — stdlib `exec.CommandContext` is sufficient.
- LACT runs as systemd service `lactd`, controlled via `sudo -n /usr/bin/systemctl stop/start lactd`.
- Shutdown sequence (state.go): Phase 1a stops llama-swap → Phase 1b stops aux containers → [INSERT: stop lactd] → checks GPU processes → if busy enters AwaitingGPUFree → polls gpu.pollInterval → when clear calls unbind.Unbind() → cuts Shelly power.
- Startup sequence (state.go): powers on via Shelly → polls nvidia-smi every gpu.pollInterval until GPU detected or timeout → [INSERT: start lactd] → starts llama-swap container → waits for health endpoint → reaches On state.
- Existing optional feature wiring pattern in main.go: `if cfg.Feature.Enabled { client := Feature.New(); machine.SetFeatureClient(client) }`.

## Notes

- The `sudo -n` flag is critical: non-interactive mode ensures the command doesn't hang waiting for a password prompt. If sudo requires authentication, Stop/Start will fail but transitions continue (logged at Warn).
- No timeout context is needed on individual systemctl calls beyond the transition's overall ctx — systemd handles these quickly.
