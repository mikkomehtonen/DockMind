# DockMind - MVP Specification

## Overview

DockMind is a lightweight daemon that manages the lifecycle of an AI
inference server running on an external GPU (eGPU).

Its primary responsibility is to provide a simple HTTP API for powering
the eGPU on/off, starting and stopping the inference backend, and
reporting the current system state.

The MVP is intentionally small. It should solve one problem well and
provide a solid foundation for future features.

------------------------------------------------------------------------

# Goals

The MVP shall:

-   Control Shelly Plug Gen3 via its local HTTP API.
-   Start and stop the `llama-swap` Docker container.
-   Monitor GPU availability using `nvidia-smi`.
-   Monitor `llama-swap` health through its REST API.
-   Expose a REST API for clients.
-   Maintain a deterministic internal state machine.

The MVP is **not** an inference proxy.

------------------------------------------------------------------------

# Non-goals

The MVP does **not**:

-   Proxy OpenAI requests
-   Manage models
-   Automatically load models
-   Authenticate users
-   Schedule GPU usage
-   Automatically power down after idle
-   Support multiple GPUs
-   Support multiple inference servers

These may be added later.

------------------------------------------------------------------------

# Architecture

``` text
              +----------------+
              |    DockMind    |
              +----------------+
               |      |      |
               |      |      +--------------------+
               |      |                           |
               |      v                           |
               |  Docker CLI                      |
               |      |                           |
               |      v                           |
               | llama-swap container             |
               |                                  |
               +--> Shelly Plug                   |
               |                                  |
               +--> nvidia-smi                    |
```

------------------------------------------------------------------------

# Responsibilities

DockMind is responsible for:

-   Managing system state
-   Powering the eGPU on/off
-   Starting/stopping llama-swap
-   Reporting health/status

DockMind is **not** responsible for inference.

------------------------------------------------------------------------

# State Machine

States:

``` text
Off
Starting
Ready
ShuttingDown
Error
```

## Off

-   Shelly power OFF
-   GPU unavailable
-   llama-swap stopped

## Starting

Sequence:

1.  Turn Shelly ON
2.  Wait until GPU appears
3.  Start llama-swap Docker container
4.  Wait until llama-swap responds successfully
5.  Transition to Ready

## Ready

Requirements:

-   Shelly ON
-   GPU detected
-   llama-swap container running
-   llama-swap health endpoint responding

## ShuttingDown

Sequence:

1.  Stop llama-swap container
2.  Wait until container exits
3.  Turn Shelly OFF
4.  Wait until GPU disappears
5.  Transition to Off

## Error

Entered whenever startup/shutdown fails.

Must include an error message explaining the reason.

------------------------------------------------------------------------

# HTTP API

## GET /status

Returns current system state.

Example:

``` json
{
  "state": "Ready",
  "gpuPresent": true,
  "gpuName": "NVIDIA GeForce RTX 5060 Ti",
  "shellyOn": true,
  "llamaSwapRunning": true,
  "llamaSwapHealthy": true,
  "lastError": null
}
```

## POST /power/on

Starts the complete system.

-   Returns immediately.
-   Startup runs asynchronously.
-   Returns `200 OK` if already Ready.
-   Returns `409 Conflict` if already Starting.

## POST /power/off

Stops the complete system.

-   Returns immediately.
-   Shutdown runs asynchronously.
-   Returns `200 OK` if already Off.
-   Returns `409 Conflict` if already ShuttingDown.

## POST /restart

Equivalent to calling:

1.  `/power/off`
2.  `/power/on`

## GET /health

Returns `200 OK` when DockMind itself is operational.

This endpoint does **not** indicate GPU readiness.

------------------------------------------------------------------------

# Monitoring

## GPU

Use:

``` bash
nvidia-smi
```

The GPU is considered available when:

-   `nvidia-smi` executes successfully
-   At least one NVIDIA GPU is detected

Polling frequency should not exceed once per second.

## llama-swap

Health endpoint:

``` text
GET /v1/models
```

Expected response:

-   HTTP 200

## Shelly

Communicate using the local HTTP RPC API.

Examples:

-   `Switch.Set`
-   `Switch.GetStatus`

Cloud connectivity must not be required.

------------------------------------------------------------------------

# Docker Integration

The inference backend runs as a Docker container.

For the MVP, DockMind shall use the Docker CLI:

``` bash
docker start llama-swap
docker stop llama-swap
docker inspect llama-swap
```

The container name must be configurable.

------------------------------------------------------------------------

# Concurrency

Only one state transition may execute at a time.

While Starting or ShuttingDown, additional power requests shall return:

``` text
HTTP 409 Conflict
```

------------------------------------------------------------------------

# Configuration

Configuration file:

``` text
config.yaml
```

Example:

``` yaml
shelly:
  address: 192.168.1.50

docker:
  container: llama-swap

llamaSwap:
  healthUrl: http://localhost:1234/v1/models

gpu:
  pollInterval: 1s

startup:
  timeout: 60s

shutdown:
  timeout: 30s
```

------------------------------------------------------------------------

# Logging

Structured logs are preferred.

Example:

``` text
INFO  Starting system
INFO  Shelly power ON
INFO  GPU detected
INFO  Starting llama-swap
INFO  llama-swap healthy
INFO  State -> Ready
```

Errors should always include the reason.

------------------------------------------------------------------------

# Suggested Project Structure

``` text
cmd/
    dockmind/

internal/
    api/
    config/
    docker/
    gpu/
    health/
    shelly/
    state/

configs/

docs/
```

------------------------------------------------------------------------

# Future Extensions (Out of Scope)

Possible future versions:

-   OpenAI-compatible proxy
-   Automatic power-on when the first inference request arrives
-   Automatic shutdown after configurable idle timeout
-   Metrics (Prometheus)
-   Web UI
-   WebSocket/SSE status updates
-   Model management
-   Authentication
-   Multiple inference backends
-   Multiple GPU hosts
-   Request queue during startup
-   Integration with OpenWebUI and Hermes as a transparent orchestration
    layer

------------------------------------------------------------------------

# Design Philosophy

Keep the MVP as small as possible.

DockMind should be a reliable orchestration daemon rather than a
feature-rich AI gateway. Every feature added to the MVP should directly
support the core objective:

> Make the AI server easy to power on, power off, and monitor through
> one simple, consistent API.
