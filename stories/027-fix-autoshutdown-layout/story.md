# Fix aux "pauses auto-shutdown" badge layout on mobile

## Context

The aux containers card renders each container as a single flex row: status dot,
container name, status text, the `pauses auto-shutdown` badge (when
`disableIdleShutdown` is set), and the Start/Stop buttons. On narrow/mobile
viewports the badge text overflows and breaks the row layout. The badge should
instead wrap onto a second row beneath the container name, while the first row
keeps `name ... status [Start] [Stop]`. The badge's current font size
(`0.75rem`) and text color (`var(--primary)`) are correct and must not change.

## Out of Scope

- Changing the badge's font size, text color, pill padding, or border styling.
- Changing any non-aux layout (components card, GPU banner, cooldown banner).
- Backend / `GET /status` field changes — `disableIdleShutdown` and
  `idleShutdownBlocked` are unchanged.
- The separate top-level "Auto-shutdown paused" indicator (driven by
  `data.idleShutdownBlocked`); only the per-row badge is moved.

## Implementation approach

All changes are in `internal/api/index.html` (the embedded HTML/CSS/JS served
at `/`). Two coordinated edits:

1. **CSS** — make `.aux__row` wrap and add a full-width badge wrapper rule:
   - `.aux__row`: add `flex-wrap: wrap;` (keep existing `display: flex`,
     `align-items: center`, `gap`, `padding`, `border-bottom`).
   - Add `.aux__badge-row { flex-basis: 100%; }` — a flex item at 100% main
     size forces a wrap onto its own line; the pill inside stays left-aligned
     under the container name. The existing `gap: 0.625rem` produces a small
     vertical gap between the two rows, which is the intended visual.
   - `.aux__badge` is unchanged (still `display: inline-block`, `0.75rem`,
     `color: var(--primary)`, pill background/border).

2. **HTML template** (the `data.auxContainers.map(...)` block) — wrap the
   badge in the new full-width wrapper and move it to the end of the row so it
   wraps after the actions instead of pushing them to a third line:
   - `badgeHtml` becomes
     `'<div class="aux__badge-row"><span class="aux__badge">pauses auto-shutdown</span></div>'`
     (only when `c.disableIdleShutdown` is true; empty string otherwise).
   - In the template literal, place `${badgeHtml}` after the
     `aux__actions` `</div>` and before the row's closing `</div>`.

Resulting row order in the DOM: dot, label (`flex: 1`), value, actions,
badge-row. With `flex-wrap: wrap`, row 1 = `dot label… value actions` and
row 2 = the badge pill, left-aligned. When `disableIdleShutdown` is false,
`badgeHtml` is empty and no wrapping occurs — the row is unchanged from today.

Edge cases:
- Container without `disableIdleShutdown`: `badgeHtml` is `''` → no
  `.aux__badge-row` element rendered, row stays single-line. No behavior
  change.
- Multiple aux containers: each row wraps independently; the
  `.aux__row:last-child { border-bottom: none; }` rule is unaffected.
- The existing test `TestWebUIAuxIdleShutdownBlocking` asserts the body
  contains `pauses auto-shutdown` and `aux__badge` — both strings remain
  present in the new template, so it stays green.

## Tasks

### Task 1 - Move the "pauses auto-shutdown" badge to a second row

The web UI is a single embedded `index.html` served at `/`; per-container rows
are rendered client-side by JS, so the served body contains the template
source and the `<style>` block. Acceptance is verified by string assertions
against that served body (the established pattern in `api_test.go`, e.g.
`TestWebUIAuxIdleShutdownBlocking`).

- `GET /` (served `index.html` body)
  - → contains the badge wrapper string `<div class="aux__badge-row"><span class="aux__badge">pauses auto-shutdown</span></div>`
  - → that `aux__badge-row` string occurs after the `aux__actions` class occurrence within the aux row template (verify by index comparison, mirroring the `pausedIdx`/`idleIdx` check in `TestWebUIAuxIdleShutdownBlocking`)
  - → the `.aux__row` CSS rule declares `flex-wrap: wrap`
  - → contains a `.aux__badge-row` CSS rule declaring `flex-basis: 100%`
  - → the `.aux__badge` CSS rule still declares `font-size: 0.75rem` and `color: var(--primary)` (unchanged)
  - → the badge wrapper is still gated by the existing `c.disableIdleShutdown ? ... : ''` ternary (the `disableIdleShutdown` token remains present in the template)
- `make test` + `make lint`
  - → both pass; existing `TestWebUIAuxIdleShutdownBlocking` stays green because `pauses auto-shutdown` and `aux__badge` strings remain present

## Notes

- This is a CSS/HTML-template-only change inside the single embedded
  `internal/api/index.html` file; no Go logic, no new dependencies, no config
  changes.
- The badge pill keeps its current appearance (size, color, pill shape); only
  its position changes from inline-in-row to a wrapped second row.
- No browser/headless rendering test exists in this repo; acceptance is
  verified by string assertions against the served HTML body (the established
  pattern in `internal/api/api_test.go`).
