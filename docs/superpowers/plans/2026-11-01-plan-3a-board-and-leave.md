# boardingparty Plan 3a: `board` and `leave` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The user runs `boardingparty board`, picks a target, and gets a localhost URL. In the browser they edit the project (sites, roles, personas, stories), start a run, watch its state, and play the recording with bugs alongside. `boardingparty leave` removes everything boardingparty installed.

**Architecture:** `board` reuses the existing target picker and first-use install, holds one port-forward to the director's admin API, and serves an embedded single-page UI on `127.0.0.1` with a reverse proxy to `/api/*`. A per-launch random token gates the local server (ruling R10). The UI is static files embedded in the CLI binary: Preact + htm as vendored ES modules, no build step. The look follows the approved prototype (`docs/superpowers/specs/2026-10-29-boardingparty-prototype.html`).

**Tech Stack:** Go 1.26 (`net/http`, `net/http/httputil`, `embed`), Preact 10 + htm 3 vendored as ESM files, plain CSS.

**Spec:** `docs/superpowers/specs/2026-10-29-boardingparty-design.md` §4.6 (one entry point, `leave`).

**Not in this plan:** scheduler, cast swimlanes, limits, supervisors, email, profiles (Plan 2); roles/limits screens beyond the project editor.

## Global Constraints

- Module `github.com/boarding-party/boardingparty`. Commits: author Tiny Systems <dev@tinysystems.io>, conventional subjects, no Co-Authored-By or "Generated with" trailers. Never commit `.idea/`.
- Every server-derived string shown in the terminal goes through `clean()`. In the browser, render text as text (never `innerHTML` / `dangerouslySetInnerHTML` with server data): agent-written bug text is untrusted.
- The local server binds `127.0.0.1` only. Every request needs the launch token: `?key=` on first load sets an HttpOnly, SameSite=Strict cookie; afterwards the cookie. Requests without either get 401. Reject requests whose `Host` isn't `127.0.0.1:<port>` or `localhost:<port>` (DNS-rebinding guard).
- `leave` never touches anything tiny owns.
- UI copy: GB English, plain sentence case, compact functional layout (inputs `px-3 py-2 text-sm`-equivalent, `rounded-md`, cards `p-4`).
- Vendored JS pinned to exact versions with a `VENDOR.md` listing source URLs and SHA-256.

## Review Focus

1. **The token leaks or isn't enforced on a path:** every route, including static files and `/api/*`, returns 401 without the cookie or key. Test in Task 3.
2. **The port-forward drops while `board` is open** (director restarts): the proxy reconnects on the next request instead of failing forever. Test in Task 3 with a fake forwarder.
3. **The video can't seek:** artifacts must support HTTP Range requests so the player can jump to a bug's moment. Test in Task 2.
4. **The project editor loses data:** saving round-trips every field (sites, extraHosts, model, roles, personas incl. traits and identity, stories). Test in Task 2 at the API level.
5. **`leave` while a run is active:** it stops the run, waits for the run session and pods to go, then deletes the director objects. Test in Task 1.

---

### Task 1: `leave` and managed-by labels

**Files:** Modify `deploy/director.yaml`; create `internal/cli/leave.go`, `internal/cli/leave_test.go`; modify `internal/cli/root.go` (register).

- Every object in `deploy/director.yaml` gets `labels: {app.kubernetes.io/managed-by: boardingparty, app.kubernetes.io/name: bp-director}` (pod template keeps `app: bp-director` too).
- `boardingparty leave [-n] [--context] [--keep-data] [--yes]`: target picker (same as other commands); show what will be deleted and ask to confirm unless `--yes` (non-TTY requires `--yes`). Then: delete run Sessions labelled `boardingparty.io/run` with foreground propagation and wait (timeout 3m) until no run sessions or `bp-run-` pods remain; delete objects labelled `app.kubernetes.io/managed-by=boardingparty` (Deployment, Service, RoleBinding, Role, ServiceAccount, and the PVC unless `--keep-data`); also delete those six by name if unlabelled (installs made before this change). Print each deletion. Idempotent: nothing to delete → "nothing to remove".
- Tests (fake clients): active run session + director → sessions deleted first, then director objects; `--keep-data` keeps the PVC; unlabelled legacy objects removed by name; non-TTY without `--yes` fails and deletes nothing; second run is a no-op.
- Commit: `feat(cli): leave removes everything boardingparty installed`.

### Task 2: Director API for the UI

**Files:** Modify `internal/api/api.go`, `internal/api/api_test.go`, `internal/store` if needed.

- `GET /api/project` → project JSON (404 with `{"error":"no project yet"}` when none). `PUT /api/project` with JSON body → validates via `project.Load` rules (refactor so JSON and YAML share validation), 204 or 400 with the validation message. Keep `POST /api/project` (YAML) for the CLI.
- `GET /api/runs/{id}/artifacts/{name}` serves with `http.ServeContent` (Range support, correct `Content-Type` for `.webm`, `.zip`, `.png`), and `Content-Disposition: inline` for video.
- Run detail adds nothing new; bugs already carry `step`.
- Tests: PUT/GET round-trips every field; PUT invalid (no sites, duplicate persona) → 400 with the message; Range request `bytes=0-9` → 206 with 10 bytes.
- Commit: `feat(api): JSON project endpoints and seekable artifacts`.

### Task 3: `board` server

**Files:** Create `internal/board/server.go`, `internal/board/server_test.go`, `internal/board/proxy.go`, `internal/cli/board.go`; register in root.

- `boardingparty board [-n] [--context] [--port 0] [--no-open]`: target picker → `ensureDirector` → open a port-forward to the director admin API (existing `forward`) → start `http.Server` on `127.0.0.1:<port>` (0 = free port) → print:
  ```
  target: <ctx> / <ns>
  ✓ director ready
  → http://127.0.0.1:<port>/?key=<32 random bytes, hex>
  ```
  and open the browser unless `--no-open` (`open`/`xdg-open`/`rundll32`; failure is not an error). Ctrl-C shuts down cleanly (server, forward).
- Token: as in Global Constraints. Host-header guard. `/api/*` reverse-proxied to the forward's base URL (`httputil.ReverseProxy`), streaming (no buffering of video). Everything else served from the embedded UI (`internal/board/ui/`, `//go:embed ui`), SPA fallback to `index.html`.
- Reconnect: the proxy uses a `Forwarder` interface; on a dial error it re-opens the port-forward once and retries the request (non-idempotent requests aren't retried; they return 502 with "director unavailable — retrying connection").
- Tests: 401 without token on `/`, `/app.js`, `/api/runs`; key sets cookie and redirects to `/` without the key; wrong Host → 403; proxy passes through status/body; dial error → reconnect then success; POST during outage → 502 without retry.
- Commit: `feat(cli): board serves the UI on localhost`.

### Task 4: UI shell, runs, run detail

**Files:** `internal/board/ui/index.html`, `app.js`, `styles.css`, `vendor/preact.module.js`, `vendor/htm.module.js`, `vendor/hooks.module.js`, `VENDOR.md`.

- Follow the prototype's tokens and layout: dark sidebar (Project, Runs), run strip at top (director state, active run, link to project editor).
- Hash routing: `#/runs`, `#/runs/<id>`, `#/project`.
- Runs: table (persona, story, state tag, started, outcome first line), "Start run" form (persona + story selects from the project; disabled with an explanation while a run is active: 409 message shown verbatim), auto-refresh every 5 s while any run is active.
- Run detail: video player for the first `.webm`; under it a bug list where each bug's step is shown and clicking a bug seeks the video if the bug text carries a `mm:ss` timestamp (else no seek); outcome, persona, story, timings; artifact download links (`trace.zip` with the `npx playwright show-trace` hint).
- Empty states: no project → "Set up your project first" linking to `#/project`; no runs → "No runs yet. Start one above."
- All server text rendered as text nodes (htm escapes by default; never set innerHTML).
- No unit tests for the UI; verification: `go test ./internal/board/...` still passes, and a manual check runs `board` against a fake director (Task 6).
- Commit: `feat(ui): runs list, start run, run detail with video`.

### Task 5: UI project editor

**Files:** `internal/board/ui/project.js` (and wiring in `app.js`).

- Form for name, sites (list), extra hosts (list), model (optional), roles (repeatable: name, description, can watch recordings), personas per role (name, traits as tags, identity key/value rows), stories per role (name, goal textarea). Add/remove rows. "Save" → `PUT /api/project`; show the director's 400 message inline at the top; success toast "Project saved".
- "Import YAML" (paste box) → client sends it to `POST /api/project` as today; "Export YAML" not needed.
- Warn (don't block) if a site isn't `https://` or looks like production (`www.`, no `staging`/`test`/`dev` in host): "Make sure this is a staging site".
- Commit: `feat(ui): project editor`.

### Task 6: Docs and a local check

**Files:** README.md, docs/e2e.md, `internal/board/fakedirector_test.go` (or a `cmd/fakedirector` dev tool if simpler).

- README "Trying it": `tiny init -n <ns>` + agent key, then `boardingparty board`; scripting commands as a short secondary section; `leave` for cleanup. GB English, plain voice, humanizer-style.
- A dev helper that serves the admin API from an in-memory store with a fake launcher (runs finish after 20 s with a sample `.webm` and a bug), so `board --dev-director` (hidden flag) can be checked in a browser without a cluster. Verify manually: start → URL → editor save → start run → run completes → video plays → bug list shows.
- Commit: `docs: board and leave`.
