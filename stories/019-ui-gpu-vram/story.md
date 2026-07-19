# UI GPU VRAM Display

## Context

The web UI currently shows a "GPU processes (preventing power off)" banner while the system is in `AwaitingGPUFree`. That wording is misleading because DockMind stops `llama-swap` before checking for blocking processes, so `llama-server` itself is not what prevents power off. Users also want visibility into how much VRAM each GPU process is using and the overall GPU memory pressure, both of which are available from `nvidia-smi`.

## Out of Scope

- Changing the shutdown logic or which processes block power off.
- Background polling or caching of GPU memory data.
- Parsing memory values into numeric units; the API preserves nvidia-smi's raw strings.
- Showing VRAM info when no GPU processes are detected.

## Implementation approach

- Extend `state.GPUProcess` with a `UsedGPUMemory` string field populated from `nvidia-smi --query-compute-apps=pid,process_name,used_gpu_memory --format=csv,noheader`.
- Add a `GPUMemory` struct (`Total`, `Used`, `Free` strings) and a `Memory(ctx)` method to the `GPUMonitor` interface, backed by `nvidia-smi --query-gpu=memory.total,memory.used,memory.free --format=csv,noheader`.
- Add `GPUMemory` to `state.StatusResponse` and populate it only when the GPU is present and at least one compute process is detected, matching the condition under which the UI banner is visible.
- Update the web UI banner label to "GPU processes" (removing "preventing power off"), render each process as `name (PID pid, usedGpuMemory)`, and add a VRAM summary line under the list.
- Update the OpenAPI spec and README status example to reflect the new fields.

## Tasks

### Task 1 - Backend GPU memory data

- `nvidia-smi --query-compute-apps=pid,process_name,used_gpu_memory` returns one CSV line per process + empty trailing line
  - → `gpu.Monitor.Processes` returns each process with `PID`, `Name`, and the raw `UsedGPUMemory` string (e.g. `12734 MiB`)
  - → lines with fewer than three fields or a non-numeric PID are skipped, preserving existing behavior
- `nvidia-smi --query-gpu=memory.total,memory.used,memory.free` returns one CSV line like `16311 MiB, 12742 MiB, 3108 MiB`
  - → `gpu.Monitor.Memory` returns a `GPUMemory` struct with raw `Total`, `Used`, and `Free` strings
  - → if the command fails or the output has fewer than three fields, `Memory` returns an error
- `state.GPUProcess` gains `UsedGPUMemory string` with JSON tag `usedGpuMemory`
- `state.GPUMonitor` gains `Memory(ctx context.Context) (GPUMemory, error)`
- `state.StatusResponse` gains `GPUMemory GPUMemory` with JSON tag `gpuMemory`
- `state.Machine.Status` probes memory only when the GPU is present and `probeGPUProcesses` returns a non-empty slice
  - → `StatusResponse.GPUMemory` is populated with the raw strings
  - → if the memory probe fails, `GPUMemory` remains the zero value (empty strings) and the failure is logged at Debug level

### Task 2 - API spec and documentation

- OpenAPI `StatusResponse` schema includes `gpuMemory` with `total`, `used`, `free` string properties
- OpenAPI `gpuProcesses` item schema includes `usedGpuMemory` string property
- README status example includes `usedGpuMemory` inside `gpuProcesses` and a `gpuMemory` object with `total`, `used`, `free`

### Task 3 - Web UI rendering

- GPU processes banner label reads `GPU processes` with no parenthetical text
- Each process renders as `${name} (PID ${pid}, ${usedGpuMemory})`
- When `gpuMemory.total` is non-empty, a summary line is shown under the process list reading `VRAM: ${used} / ${total} used (${free} free)`
- The banner remains hidden when `gpuProcesses` is empty, and the VRAM summary is hidden when `gpuMemory.total` is empty

## Bootstrap

No new dependencies. Existing commands still apply:

```bash
make build
make test
make lint
```

## Technical Context

- Go 1.24.4, stdlib `testing` only, single external dependency `gopkg.in/yaml.v3`.
- `nvidia-smi` is not installed on the dev host; all `gpu` tests inject a fake `execFunc`.
- `state` tests inject a fake `GPUMonitor` that must implement the new `Memory` method.
- The UI is a single embedded `index.html` served by `internal/api`; it polls `/status` once per second.

## Notes

- The raw-string approach keeps the API robust against `N/A` values from `nvidia-smi` and avoids unit-conversion complexity.
- Memory is probed only when processes are present to avoid an extra `nvidia-smi` invocation on every poll while the GPU is idle.
