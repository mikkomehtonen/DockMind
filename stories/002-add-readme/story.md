# Add README.md

## Context

DockMind has a complete MVP implementation (story 001-mvp-core) and detailed
documentation in `docs/` (MVP specification, product overview, learnings), but
no `README.md` at the repository root. A new visitor who opens the repo on
GitHub or clones it has no entry point — they must discover the `docs/`
directory and `Makefile` themselves. This story adds a concise `README.md`
that orients the reader: what DockMind is, how to build and run it, the API at
a glance, and where to find full details. It deliberately stays short and links
to the existing `docs/` for anything beyond a quick start.

## Out of Scope

- Duplicating the full request-response matrix, state machine transition
  sequences, or complete config schema — these live in
  `docs/DockMind_MVP_Specification.md` and the README links to them.
- A license section — this is a personal project; no license file exists or is
  planned.
- A contributing guide, code of conduct, or changelog.
- Translations, badges, or a table of contents.
- Documenting `docs/learnings.md` — it is an internal dev journal, not
  user-facing.

## Implementation approach

### File location

Create `/app/README.md` at the repository root.

### README sections (concise — link out for details)

1. **Title + one-line description** — "DockMind" plus the opening sentence from
   `docs/product.md` (a lightweight daemon that manages the lifecycle of an AI
   inference server running on an external GPU).
2. **Features** — 4–6 bullet points summarizing eGPU power control, inference
   backend lifecycle, state machine, REST API, GPU monitoring, and health
   monitoring. Link to `docs/product.md` for the full feature list.
3. **Quick Start** — prerequisites (Go 1.24, Docker CLI, `nvidia-smi`, a
   Shelly Plug Gen3 on the LAN, and a `llama-swap` Docker container), then:
   - `make build` (produces `./dockmind`)
   - configure: copy `configs/config.yaml` to `./config.yaml` and edit
   - run: `./dockmind --config config.yaml` (default config path is
     `./config.yaml`)
   - `make test` and `make lint` for development
4. **API Endpoints** — a brief table (Method, Path, Description) listing all
   five routes. Link to `docs/DockMind_MVP_Specification.md` for the full
   request-response matrix and state machine.
5. **Configuration** — a fenced ```yaml block with the config example (same
   content as `configs/config.yaml`), plus a one-line note that optional
   fields have defaults and a link to the spec for the full schema.
6. **Status Example** — a fenced ```json block showing a `GET /status`
   response with all seven `StatusResponse` fields.
7. **Project Structure** — a brief tree matching the actual layout
   (`cmd/dockmind`, `internal/{api,config,docker,gpu,health,shelly,state}`,
   `configs`, `docs`). Link to the spec for details.
8. **Further Reading** — links to `docs/DockMind_MVP_Specification.md` and
   `docs/product.md`.

No **License** section.

### Link format

Use relative links (e.g.
`[MVP Specification](docs/DockMind_MVP_Specification.md)`) so they resolve on
GitHub from the repo root.

### Automated validation test

Create `/app/readme_test.go` with `package dockmind_test`. It imports
`github.com/dockmind/dockmind/internal/config` (allowed: the repo root is the
parent of `internal/`, satisfying Go's internal-package import rule — verified
empirically that a root-level test-only package with no non-test `.go` files
compiles and runs under `go test ./...`). The test reads `README.md` from the
repo root via `os.ReadFile("README.md")` (`go test` runs with each package's
directory as the working directory, so CWD is `/app`) and asserts every
acceptance criterion below.

For the config example, the test extracts the first fenced ```yaml code block
using a regex capturing the content between ` ```yaml ` and the next ` ``` `,
writes it to a temp file, and calls `config.Load` on it. No new dependencies —
stdlib only (`os`, `regexp`, `strings`, `testing`).

## Tasks

### Task 1 - README.md with automated validation

- README.md does not exist at repo root + create it
  - → `readme_test.go` reads `./README.md` with `os.ReadFile` and gets no error
- README links to the MVP specification
  - → README contains the string `docs/DockMind_MVP_Specification.md`
- README links to the product overview
  - → README contains the string `docs/product.md`
- README documents the build command
  - → README contains `make build`
- README documents the test command
  - → README contains `make test`
- README documents the lint command
  - → README contains `make lint`
- README documents the --config flag and its default path
  - → README contains both `--config` and `./config.yaml`
- README contains a fenced yaml config example
  - → test extracts the first ```yaml block and `config.Load` returns no error
- config example includes all required fields
  - → after Load, `cfg.Shelly.Address`, `cfg.Docker.Container`, and `cfg.LlamaSwap.HealthURL` are all non-empty
- README documents all five API routes
  - → README contains `/status`, `/power/on`, `/power/off`, `/restart`, and `/health`
- README contains a GET /status JSON example
  - → README contains the field names `state`, `gpuPresent`, `gpuName`, `shellyOn`, `llamaSwapRunning`, `llamaSwapHealthy`, and `lastError`
- README has no License section
  - → no line in README matches the regex `^#+\s*[Ll]icense`
- README stays concise (delegates the full request-response matrix to the spec)
  - → README does not contain `ResultAlreadyInState` or `ResultConflict`
- `make test` run from repo root
  - → `readme_test.go` passes alongside all existing tests
- `make lint` run from repo root
  - → `gofmt -l .` and `go vet ./...` report no issues

## Notes

- **Conciseness is intentional**: the README is a quick-start overview. Full
  API semantics, state machine transitions, and the complete config schema
  belong in `docs/DockMind_MVP_Specification.md`; the README links there. This
  is enforced by the AC that rejects internal enum names (`ResultConflict`,
  `ResultAlreadyInState`) appearing in the README.
- **No license**: per the project owner, this is a personal project with no
  license file. Do not add a License section or a `LICENSE` file.
- **Relative links**: use repo-root-relative paths (`docs/...`) so links work
  on GitHub. Verified by the string-presence ACs.
- **Test CWD**: `go test` executes with each package's directory as the
  working directory, so `os.ReadFile("README.md")` resolves to the repo-root
  README when the test lives at `/app/readme_test.go`. Verified empirically.
