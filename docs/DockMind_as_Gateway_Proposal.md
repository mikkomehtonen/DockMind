# Design Proposal: DockMind as an Intelligent OpenAI Gateway

## Objective

The final design should be simple, reliable, maintainable, and suitable
for production use.

------------------------------------------------------------------------

# Background

DockMind is a Go application that currently controls an external eGPU
(Mantiz Venus + RTX 5060 Ti) and a llama-swap service.

Today the user manually powers the GPU on and off from the DockMind UI.

The goal is to evolve DockMind into an intelligent gateway so that
clients never communicate directly with llama-swap.

Instead, all clients (Hermes, OpenWebUI, curl, custom applications,
etc.) communicate with DockMind using the OpenAI-compatible API.

DockMind becomes responsible for automatically starting and stopping the
AI backend.

------------------------------------------------------------------------

# Desired Behaviour

## Automatic startup

When the first OpenAI request arrives:

1.  Check whether the backend is already available.
2.  If it is not:
    -   Power on the eGPU using the Shelly device.
    -   Wait until the GPU becomes available.
    -   Start the llama-swap docker container.
    -   Wait until the backend reports healthy.
    (i.e. the same procedure as currently when the user starts the system from the UI)
3.  Forward the original request to llama-swap.
4.  Return the backend response to the client.

The client should not need to know that the backend was started
automatically.

## Automatic shutdown

DockMind should continuously monitor activity.

If the backend has been idle for a configurable amount of time (for
example, 30 minutes):

1.  Stop llama-swap.
2.  Power off the eGPU.

Shutdown must never interrupt active requests.

# Technology Stack

The design should assume:

-   Go
-   net/http
-   Standard library whenever possible
-   OpenAI-compatible REST API
-   Reverse proxy
-   docker for managing llama-swap
-   Shelly HTTP API for power control

Avoid unnecessary third-party dependencies.

# Topics to Cover

Please produce a detailed design covering at least the following areas.

## 1. High-level Architecture

Describe: - major components - responsibilities - interactions -
dependency relationships

Include an architecture diagram if appropriate.

## 2. Go Package Structure

Recommend a package layout and explain the responsibility of each
package.

## 3. State Machine

Design a complete state machine including: - all states - valid
transitions - error states - recovery paths

Example states: - Off - StartingPower - WaitingForGPU -
StartingBackend - Ready - Busy - Idle - Stopping - Error

Discuss whether additional states are needed.

## 4. Startup Sequence

Describe the complete startup flow, including: - request arrival -
startup sequence - backend health verification - request forwarding

Only one startup operation should ever execute at a time. Explain how
concurrent requests wait for the existing startup operation.

## 5. Reverse Proxy

Describe how DockMind should proxy OpenAI requests, including: - request
forwarding - response forwarding - streaming responses - HTTP headers -
error handling - timeout handling

The design should support Server-Sent Events (SSE) and OpenAI streaming
responses without buffering the entire response.

## 6. Idle Shutdown

Describe how inactivity should be tracked, including: - active request
counting - last activity timestamp - configurable timeout - safe
shutdown conditions

Explain how race conditions between new requests and shutdown should be
avoided.

## 7. Synchronization

Discuss: - mutexes - atomic variables - channels - goroutines - context
cancellation

Explain why each synchronization primitive is appropriate.

## 8. Error Handling

Describe behaviour when: - Shelly is unreachable - GPU never appears -
llama-swap fails to start - backend health checks fail - proxy
connection fails - backend crashes while serving requests

Discuss retry strategies and user-visible error responses.

## 9. Configuration

Identify all configuration values, for example: - backend URL - startup
timeout - idle timeout - health check interval - Shelly address -
systemd service name - logging level

Recommend sensible defaults.

## 10. Logging

Describe the logging strategy.

Explain what should be logged at: - INFO - WARN - ERROR - DEBUG

Avoid excessive log noise while keeping troubleshooting practical.

## 11. Testing Strategy

Recommend: - unit tests - integration tests - failure simulations -
concurrency tests - startup/shutdown tests - proxy tests

Identify the most critical scenarios.

## 12. Incremental Implementation Plan

Break the implementation into small milestones.

Each milestone should leave DockMind in a working state.

For every milestone include: - objectives - dependencies - risks -
expected outcome

The roadmap should minimize implementation risk.

# Design Principles

Prioritize: - simplicity - reliability - maintainability -
extensibility - clear separation of responsibilities

Avoid unnecessary abstractions and over-engineering.

Prefer the Go standard library whenever practical.

# Final Review

Before finishing, critically review the proposed architecture.

Identify: - unnecessary complexity - possible simplifications -
architectural risks - alternative approaches worth considering

The goal is to produce the simplest architecture that fully satisfies
the requirements before any production code is written.
