# boardingparty Plan 1: Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One persona runs one story end to end on a real cluster: the director starts a tiny Session with the worker image, the agent drives Chromium through Playwright MCP, talks to the director over MCP, uploads its video and trace, and finishes the run with an outcome you can list from the CLI.

**Architecture:** tiny gains a generic `spec.mcpServers` (Tasks 1–2). boardingparty is one Go module with two binaries: `director` (in-cluster: SQLite store, HTTP API, MCP server, tiny Session launcher) and `boardingparty` (CLI reaching the director through a Kubernetes port-forward). The worker is a Playwright base image that tiny injects its payload into. No scheduler, Mailpit, UI or install command yet; those are Plans 2–3.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (no cgo), `github.com/modelcontextprotocol/go-sdk`, `sigs.k8s.io/controller-runtime` client, `k8s.io/client-go` port-forward, `github.com/spf13/cobra`, `sigs.k8s.io/yaml`, `mcr.microsoft.com/playwright:<latest stable>-noble` + `@playwright/mcp` (pin both exact versions in Task 3; the tag shown in Task 3 is a placeholder).

**Spec:** `docs/superpowers/specs/2026-10-29-boardingparty-design.md`

**Later plans (not here):** Plan 2 = scheduler, activity patterns, handoffs, waiting/stuck states (§5, §5.1), limits and budget, Mailpit inboxes, bug grouping, supervisors' recording tools, S3 storage and retention. Plan 3 = `boardingparty install`, embedded web UI from the prototype, `ui` command.

## Global Constraints

- Go module path: `github.com/tiny-systems/boardingparty` (my call; change before the first push if the repo lives elsewhere).
- Commits: author `Tiny Systems <dev@tinysystems.io>`, conventional prefixes, **no Co-Authored-By or "Generated with" trailers**.
- tiny changes must be generic and useful to any tiny user (spec §2, anti-goals). Nothing boardingparty-specific lands in tiny.
- Director: one replica, state in SQLite on a PVC, no Ingress, no Service type other than ClusterIP (spec §4.1).
- Personas never own a pod. One tiny Session per run, deleted when the run finishes (spec §4.2).
- Agents may only reach the configured staging hosts and the model API (spec §2, anti-goals). Never point a project at production.
- All identity data is synthetic.
- Spec deviation, recorded: the per-run token travels in the MCP URL path (`/mcp/run/{token}`), not a header, so `spec.mcpServers` needs no Secret-backed headers. Update spec §4.3 in Task 1's commit.

## Review Focus

1. **A run whose agent never calls `finish_run`** (crash, usage limit, wanders off): expect the launcher to end it as `failed` with reason "no finish" after `RunTimeout` (default 20 min) and delete the Session. Test in Task 7.
2. **A reused or forged run token**: expect every MCP tool and upload to answer "unknown or finished run" once a run is finished, and for random strings. Tests in Tasks 5 and 6.
3. **`spec.mcpServers` with a name or arg containing spaces or quotes**: expect the API server to reject it at admission (CRD validation), so the entrypoint's word-splitting can never be injected into. Test in Task 1.
4. **Director restart mid-run**: expect running runs to be re-adopted from SQLite on startup (Session still exists → keep watching; Session gone → `failed`, "lost on restart"). Test in Task 7.
5. **An oversized or path-traversing artifact upload** (`../../etc/x`, 2 GB): expect 400 for names outside `[a-z0-9._-]{1,64}`, 413 above `MaxArtifactBytes` (default 500 MB). Test in Task 5.

---

### Task 1: tiny: `spec.mcpServers` on Session, rendered into env

Repo: `/Users/mtrofimenko/Work/Current/tiny-systems/tiny`

**Files:**
- Modify: `api/v1alpha1/session_types.go` (add field + type after `Inbox`)
- Modify: `internal/workload/workload.go` (agent container `Env`)
- Create: `internal/workload/mcp.go`
- Test: `internal/workload/mcp_test.go`
- Regenerate: `make manifests generate`
- Modify: `/Users/mtrofimenko/Work/boardingparty/docs/superpowers/specs/2026-10-29-boardingparty-design.md` §4.3 (token in URL path, no header)

**Interfaces:**
- Produces (API):
  ```go
  // MCPServer is an extra MCP server the agent registers next to tiny's own.
  type MCPServer struct {
      // +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,31}$`
      Name string `json:"name"`
      // HTTP endpoint. Mutually exclusive with Command.
      // +kubebuilder:validation:Pattern=`^https?://[^\s"'\\]+$`
      // +optional
      URL string `json:"url,omitempty"`
      // Stdio server started inside the agent container.
      // +kubebuilder:validation:Pattern=`^[^\s"'\\]+$`
      // +optional
      Command string `json:"command,omitempty"`
      // +kubebuilder:validation:items:Pattern=`^[^\s"'\\]+$`
      // +optional
      Args []string `json:"args,omitempty"`
  }
  // SessionSpec: MCPServers []MCPServer `json:"mcpServers,omitempty"` (+listType=map, +listMapKey=name, MaxItems=8)
  ```
  plus a CEL rule `+kubebuilder:validation:XValidation:rule="has(self.url) != has(self.command)",message="set exactly one of url or command"` and reject name `tiny`.
- Produces (env on the agent container, only when the list is non-empty):
  - `TINY_MCP_CONFIG`: claude `--mcp-config` JSON, `{"mcpServers":{"<name>":{"type":"http","url":...}|{"command":...,"args":[...]}}}`
  - `TINY_CODEX_MCP_ARGS`: space-separated `-c` overrides, e.g. `-c mcp_servers.pw.command="npx" -c mcp_servers.pw.args=["@playwright/mcp","--headless"] -c mcp_servers.bp.url="http://director:8080/mcp/run/abc"`
- Function: `func mcpEnv(servers []agentsv1.MCPServer) []corev1.EnvVar` in `internal/workload/mcp.go`.

- [ ] **Step 1: Write failing tests** in `internal/workload/mcp_test.go`:
  - `TestMCPEnvEmptyListAddsNothing`: `mcpEnv(nil)` returns `nil`; `podFor(t, testSession("u1"))` agent container has no `TINY_MCP_CONFIG`.
  - `TestMCPEnvHTTPAndStdio`: servers `{Name:"bp", URL:"http://director:8080/mcp/run/abc"}` and `{Name:"pw", Command:"npx", Args:[]string{"@playwright/mcp","--headless"}}` → `TINY_MCP_CONFIG` unmarshals to exactly `{"mcpServers":{"bp":{"type":"http","url":"http://director:8080/mcp/run/abc"},"pw":{"command":"npx","args":["@playwright/mcp","--headless"]}}}`; `TINY_CODEX_MCP_ARGS` equals `-c mcp_servers.bp.url="http://director:8080/mcp/run/abc" -c mcp_servers.pw.command="npx" -c mcp_servers.pw.args=["@playwright/mcp","--headless"]` (servers in spec order).
  - `TestMCPServersRejectsUnsafeValues` (envtest, alongside the existing API tests or a new `api/v1alpha1` envtest): creating a Session with `Args: []string{"a b"}`, `Name: "Bad Name"`, `Name: "tiny"`, or both URL and Command set fails with a validation error.
- [ ] **Step 2: Run** `go test ./internal/workload/ -run MCP -v`. Expected: FAIL, `mcpEnv` undefined.
- [ ] **Step 3: Implement** the type, the `mcpEnv` function (encoding/json for the claude value; codex values are `"<v>"` strings and a `[...]` TOML array of quoted strings), and append `mcpEnv(s.Spec.MCPServers)...` to the agent container `Env`. Run `make manifests generate`.
- [ ] **Step 4: Run** `make test`. Expected: PASS, including the existing confine/workspace tests.
- [ ] **Step 5: Commit** in tiny: `feat: sessions can register extra MCP servers (spec.mcpServers)`. Commit the spec §4.3 edit in boardingparty: `docs: run token travels in the MCP URL`.

### Task 2: tiny: entrypoint passes the extra servers to claude and codex; release

**Files:**
- Modify: `images/agent/entrypoint.sh` (lines ~275, 291, 294, 310, 320: every `claude`/`codex` launch, fresh and resume)
- Modify: tiny docs page for sessions (wherever `spec.image` is documented) with a `mcpServers` example

**Interfaces:**
- Consumes: `TINY_MCP_CONFIG`, `TINY_CODEX_MCP_ARGS` from Task 1.

- [ ] **Step 1: Edit launches.** claude: append `${TINY_MCP_CONFIG:+--mcp-config "$TINY_MCP_CONFIG"}`. codex: append `$TINY_CODEX_MCP_ARGS` unquoted (safe: Task 1 validation forbids whitespace and quotes). Add a comment saying so. Extra servers' tools are pre-allowed by `bypassPermissions`; no settings.json change.
- [ ] **Step 2: Shell check.** Run `sh -n images/agent/entrypoint.sh && shellcheck -s sh images/agent/entrypoint.sh`. Expected: no new findings beyond the existing baseline.
- [ ] **Step 3: Build and push** the agent and controller images per tiny's release process (`Makefile`/`CONTRIBUTING.md`), and tag a tiny release.
- [ ] **Step 4: Verify on the cluster.** Upgrade tiny; create a session with `spec.mcpServers: [{name: everything, command: npx, args: ["-y","@modelcontextprotocol/server-everything"]}]` and task "List your MCP servers and call the echo tool of the 'everything' server with 'ping', then stop." Expected: the session's activity shows the echo result. Repeat with `--agent codex`.
- [ ] **Step 5: Commit** `feat: entrypoint wires spec.mcpServers into claude and codex`.

### Task 3: Worker image spike: Chromium and Playwright MCP inside a confined tiny session

Throwaway verification of spec §6.2 before the director exists. Keeps the image.

**Files:**
- Create: `/Users/mtrofimenko/Work/boardingparty/worker/Dockerfile`
- Create: `/Users/mtrofimenko/Work/boardingparty/worker/README.md` (what the image contains, why `--no-sandbox`)

**Interfaces:**
- Produces: image `ghcr.io/tiny-systems/boardingparty-worker:<tag>` with `git`, `curl`, `node`, Chromium from the Playwright base, and `@playwright/mcp` installed globally at a pinned version. Working MCP server definition for later tasks:
  `{name: pw, command: playwright-mcp, args: ["--headless","--isolated","--no-sandbox","--browser=chromium","--output-dir=/workspace/out","--save-video=1280x800","--save-trace"]}`
  (confirm exact flag names against `playwright-mcp --help` of the pinned version and record them in the README).

- [ ] **Step 1: Write the Dockerfile.** `FROM mcr.microsoft.com/playwright:v1.56.0-noble`, add `git curl`, `npm i -g @playwright/mcp@<pinned>`, create uid 1000-owned `/workspace/out` parent paths only if the base image needs it.
- [ ] **Step 2: Build and push** `boardingparty-worker:spike`.
- [ ] **Step 3: Run it under tiny.** `tiny new --image ghcr.io/tiny-systems/boardingparty-worker:spike` with `mcpServers` = the `pw` server above, the hostname allow-list containing `demo.playwright.dev` and the model API, and task: "Open https://demo.playwright.dev/todomvc, add three todos, tick one, take a screenshot, then stop." Expected: the session finishes; `/workspace/out` contains a `.webm` video and a trace zip; no Chromium sandbox or seccomp crash in the pod log.
- [ ] **Step 4: If Chromium fails under the restricted profile**, record the exact error in the README, try `--unconfined` once to confirm it's the profile, and stop and report. Don't widen tiny's defaults.
- [ ] **Step 5: Commit** in boardingparty: `feat: worker image (Playwright + Playwright MCP) for tiny sessions`.

### Task 4: boardingparty module, store, and project config

**Files:**
- Create: `go.mod`, `Makefile` (`test`, `build`, `image`), `.gitignore`
- Create: `internal/store/store.go`, `internal/store/schema.sql` (embedded), `internal/store/store_test.go`
- Create: `internal/project/project.go`, `internal/project/project_test.go`, `examples/todomvc.yaml`

**Interfaces:**
- Produces:
  ```go
  package project
  type Project struct {
      Name       string   `json:"name"`
      Sites      []string `json:"sites"`       // staging URLs; hosts become the allow-list
      ExtraHosts []string `json:"extraHosts"`
      Model      string   `json:"model"`       // optional override
      Roles      []Role   `json:"roles"`
  }
  type Role struct {
      Name               string    `json:"name"`
      Description        string    `json:"description"`
      CanWatchRecordings bool      `json:"canWatchRecordings"`
      Personas           []Persona `json:"personas"` // explicit in Plan 1; generated in Plan 2
      Stories            []Story   `json:"stories"`
  }
  type Persona struct {
      Name     string            `json:"name"`
      Traits   []string          `json:"traits"`   // e.g. "impatient", "phone"
      Identity map[string]string `json:"identity"` // synthetic: email, password, address…
  }
  type Story struct{ Name, Goal string }
  func Load(b []byte) (Project, error) // YAML; validates
  ```
  ```go
  package store
  type RunState string // "pending","running","done","stuck","failed"
  type Run struct {
      ID, Token, Persona, Role, Story string
      State    RunState
      Outcome  string // agent's summary or failure reason
      Session  string // tiny Session name
      Started, Finished time.Time
  }
  type Bug struct{ ID, RunID, Summary, Step, Evidence, Severity, Note string; Created time.Time }
  type Store struct{ /* *sql.DB */ }
  func Open(path string) (*Store, error) // applies schema.sql
  func (s *Store) PutProject(p project.Project) error         // replaces the single project
  func (s *Store) Project() (project.Project, error)
  func (s *Store) PersonaState(persona string) (json.RawMessage, error) // nil if none
  func (s *Store) SavePersonaState(persona string, state json.RawMessage) error
  func (s *Store) CreateRun(r Run) error
  func (s *Store) RunByToken(token string) (Run, error) // ErrNotFound
  func (s *Store) UpdateRun(r Run) error
  func (s *Store) Runs(limit int) ([]Run, error)        // newest first
  func (s *Store) ActiveRuns() ([]Run, error)           // pending|running
  func (s *Store) AddBug(b Bug) error
  func (s *Store) Bugs(runID string) ([]Bug, error)
  var ErrNotFound = errors.New("not found")
  ```

- [ ] **Step 1: Write failing tests.** `project_test.go`: `TestLoadRejects` table: empty sites, a site that isn't `https://`, two roles with one name, a persona with no name, a story with empty goal; `TestLoadExample` loads `examples/todomvc.yaml` (one role "visitor", one persona, one story "Add three todos, complete one, check the counter says 2 items left"). `store_test.go` (temp dir DB): `TestRunRoundTrip`, `TestRunByTokenUnknown` → `ErrNotFound`, `TestActiveRunsExcludesFinished`, `TestPersonaStateRoundTrip`, `TestProjectReplace`.
- [ ] **Step 2: Run** `go test ./internal/...`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement.** SQLite via `modernc.org/sqlite`, WAL mode, `busy_timeout=5000`; project stored as one JSON row.
- [ ] **Step 4: Run** `go test ./internal/...`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: store and project config`.

### Task 5: Runs and artifacts over HTTP

**Files:**
- Create: `internal/runs/runs.go`, `internal/runs/runs_test.go`
- Create: `internal/artifacts/fs.go`, `internal/artifacts/fs_test.go`
- Create: `internal/api/api.go`, `internal/api/api_test.go`

**Interfaces:**
- Consumes: `store.Store`, `project.Project` (Task 4).
- Produces:
  ```go
  package artifacts
  type Store interface {
      Put(runID, name string, r io.Reader) (int64, error)
      Open(runID, name string) (io.ReadCloser, error)
      List(runID string) ([]string, error)
  }
  func NewFS(root string, maxBytes int64) Store // ErrTooLarge, ErrBadName
  ```
  ```go
  package runs
  type Service struct{ Store *store.Store; Artifacts artifacts.Store; Launcher Launcher }
  type Launcher interface {
      Start(ctx context.Context, r store.Run, p project.Project) (session string, err error)
      Stop(ctx context.Context, session string) error
  }
  func (s *Service) Start(ctx context.Context, persona, story string) (store.Run, error) // 32-byte random hex token
  func (s *Service) Finish(ctx context.Context, token string, state store.RunState, outcome string) error // ErrNotActive for unknown/finished; calls Launcher.Stop
  func (s *Service) Active(token string) (store.Run, error) // ErrNotActive
  ```
  HTTP (`internal/api`): `POST /api/project` (YAML body), `POST /api/runs` `{"persona","story"}` → Run JSON, `GET /api/runs`, `GET /api/runs/{id}` (run + bugs + artifact names), `GET /api/runs/{id}/artifacts/{name}`, and for workers `PUT /artifacts/{token}/{name}` (raw body).

- [ ] **Step 1: Write failing tests.** `fs_test.go`: `TestPutRejectsBadNames` (`../x`, `a/b`, empty, 65 chars, uppercase) → `ErrBadName`; `TestPutTooLarge` with maxBytes 10 → `ErrTooLarge` and no file left behind. `runs_test.go` with a fake Launcher: `TestStartUnknownPersonaOrStory` errors; `TestFinishTwiceIsNotActive`; `TestFinishStopsSession`. `api_test.go` (`httptest`): `TestWorkerUploadNeedsActiveToken` (random token → 404, finished run token → 404, active → 201), `TestUploadOversizeIs413`, `TestBadNameIs400`.
- [ ] **Step 2: Run** `go test ./internal/...`. Expected: FAIL.
- [ ] **Step 3: Implement.** `MaxArtifactBytes` default 500 MB (`http.MaxBytesReader`). Name pattern `^[a-z0-9._-]{1,64}$`.
- [ ] **Step 4: Run** `go test ./internal/...`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: runs and artifact uploads`.

### Task 6: MCP server for workers

**Files:**
- Create: `internal/mcpserver/server.go`, `internal/mcpserver/server_test.go`

**Interfaces:**
- Consumes: `runs.Service` (Task 5), `store.Store` (Task 4).
- Produces: `func Handler(svc *runs.Service, st *store.Store) http.Handler`, mounted at `/mcp/run/{token}` (streamable HTTP, go-sdk). Tools, all scoped to the token's run:
  - `next_assignment()` → `{persona: {name, role, traits, identity}, story: {name, goal}, sites: [...], personaState: <json|null>, uploadURL: "http://<director>/artifacts/<token>/"}`
  - `persona_save(state: object)` → `"saved"`
  - `report_bug(summary, step, evidence, severity_guess, note)` → bug id; `severity_guess` ∈ `high|medium|low`
  - `finish_run(outcome: "done"|"stuck"|"failed", summary)` → `"finished"`; afterwards every tool on this token returns the error text `unknown or finished run`.

- [ ] **Step 1: Write failing tests** using the go-sdk in-memory client against `Handler`: `TestNextAssignmentReturnsPersonaAndStory`, `TestUnknownTokenEveryToolErrors` (all four tools, text `unknown or finished run`), `TestFinishThenToolsError`, `TestReportBugStored`, `TestFinishRejectsBadOutcome`.
- [ ] **Step 2: Run** `go test ./internal/mcpserver/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement** with `github.com/modelcontextprotocol/go-sdk/mcp`; one `mcp.Server` per request built around the token (cheap), or one server reading the token from the request context; builder's choice.
- [ ] **Step 4: Run** `go test ./internal/mcpserver/ -v`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: worker MCP tools`.

### Task 7: Launcher: one tiny Session per run, and the run watchdog

**Files:**
- Create: `internal/launcher/tiny.go`, `internal/launcher/prompt.go`, `internal/launcher/tiny_test.go`
- Create: `internal/runs/watch.go`, `internal/runs/watch_test.go`

**Interfaces:**
- Consumes: `runs.Launcher` (Task 5); tiny's `agentsv1.Session` (import `github.com/tiny-systems/tiny/api/v1alpha1`, pinned to the Task 2 release).
- Produces:
  ```go
  type Tiny struct {
      Client      client.Client
      Namespace   string
      WorkerImage string
      DirectorURL string        // in-namespace, e.g. http://bp-director:8080
      Model       string
  }
  func (t *Tiny) Start(ctx context.Context, r store.Run, p project.Project) (string, error)
  func (t *Tiny) Stop(ctx context.Context, session string) error // delete; NotFound is success
  func (t *Tiny) Phase(ctx context.Context, session string) (agentsv1.SessionPhase, bool, error) // exists?
  func Prompt(r store.Run) string // the task text
  ```
  ```go
  func (s *Service) Watch(ctx context.Context, every, timeout time.Duration) // loop
  func (s *Service) Reconcile(ctx context.Context, now time.Time, timeout time.Duration) error // one pass, testable
  ```
  Session: name `bp-run-<first 8 of run ID>`, labels `boardingparty.io/run=<id>`, `spec.image=WorkerImage`, `spec.model=Model`, `spec.mcpServers=[{name: bp, url: DirectorURL+"/mcp/run/"+token}, <pw server from Task 3>]`, `spec.task=Prompt(r)`.
  `Prompt` content (fixed text, persona-neutral; the agent learns everything else from `next_assignment`): call `next_assignment`; act as that persona through the browser tools only, never by calling the site's API; use only the listed sites; behave per the traits; when the story is done or you cannot get further, upload every file in `/workspace/out` with `curl -T <file> <uploadURL><name>`, call `report_bug` for each problem you saw, call `persona_save` with your storage state and notes, then `finish_run`; if there is nothing you can do right now, call `finish_run` with outcome `stuck` and say why immediately. Do not poll.

- [ ] **Step 1: Write failing tests.** `tiny_test.go` with controller-runtime fake client: `TestStartCreatesSessionWithBothMCPServers` (asserts name, label, image, the `bp` URL contains the token, the `pw` server args), `TestStopNotFoundIsNil`, `TestPromptMentionsFinishAndNoPolling` (contains `finish_run`, `Do not poll`). `watch_test.go` with a fake launcher and fixed clock: `TestReconcileTimesOutSilentRun` (running, started 21 min ago, timeout 20 min → `failed`, outcome `no finish within 20m0s`, Stop called), `TestReconcileSessionGoneFailsRun` (outcome `lost on restart`), `TestReconcileLeavesHealthyRun`, `TestReconcileSessionFailedPhase` → run `failed`, outcome includes the Session message.
- [ ] **Step 2: Run** `go test ./internal/... -v`. Expected: FAIL.
- [ ] **Step 3: Implement.** Add `Phase` to the `Launcher` interface in Task 5's package. Default `RunTimeout` 20 min, poll every 15 s.
- [ ] **Step 4: Run** `go test ./internal/...`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: launch runs as tiny sessions; watchdog`.

### Task 8: Director binary and manifests

**Files:**
- Create: `cmd/director/main.go`
- Create: `deploy/director.yaml` (ServiceAccount `bp-director`, Role + RoleBinding: `agents.tinysystems.io` `sessions` get/list/watch/create/delete in the namespace only; PVC `bp-director-data` 5Gi; Deployment 1 replica, strategy `Recreate`, uid 1000, restricted security context; ClusterIP Service `bp-director` port 8080)
- Create: `Dockerfile.director` (distroless static)

**Interfaces:**
- Consumes: everything above. Flags/env: `--data=/data`, `--namespace` (downward API), `--worker-image`, `--director-url=http://bp-director:8080`, `--model`, `--run-timeout=20m`, `--max-artifact-bytes`.
- Produces: one HTTP server on `:8080` serving `/api/...`, `/artifacts/...`, `/mcp/run/{token}`, and `/healthz`.

- [ ] **Step 1: Write a failing smoke test** `cmd/director/main_test.go`: `TestServerRoutes` builds the mux with an in-temp store and fake launcher and checks `/healthz` → 200, `POST /api/project` with `examples/todomvc.yaml` → 204, `POST /api/runs` → 201.
- [ ] **Step 2: Run** `go test ./cmd/director/`. Expected: FAIL.
- [ ] **Step 3: Implement** `main` (wire store, artifacts FS under `/data/artifacts`, runs service, tiny launcher, watchdog goroutine, re-adopt on startup = first `Reconcile`), Dockerfile, manifests.
- [ ] **Step 4: Run** `go test ./... && make image`. Expected: PASS, image builds.
- [ ] **Step 5: Commit** `feat: director binary and manifests`.

### Task 9: CLI over port-forward

**Files:**
- Create: `cmd/boardingparty/main.go`, `internal/cli/{root,apply,run,runs,get}.go`, `internal/cli/forward.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: director HTTP API (Task 5).
- Produces commands (global flags `--context`, `--namespace`, defaulting to the current kube context and `bp-<project>` where applicable):
  - `boardingparty apply -f project.yaml`
  - `boardingparty run <persona> <story>` → prints run ID, then `--wait` blocks until finished and prints the outcome
  - `boardingparty runs` → table: ID, persona, story, state, age, outcome
  - `boardingparty get <run-id> [--out dir]` → prints run + bugs, downloads artifacts into `dir`
  `func forward(ctx context.Context, cfg *rest.Config, ns, svc string, port int) (baseURL string, stop func(), err error)` using client-go `portforward` to a ready pod behind the Service.

- [ ] **Step 1: Write failing tests** for the HTTP client half only (port-forward is covered by Task 10): `TestRunsTableFormatting` and `TestGetDownloadsArtifacts` against an `httptest` server.
- [ ] **Step 2: Run** `go test ./internal/cli/`. Expected: FAIL.
- [ ] **Step 3: Implement** with cobra.
- [ ] **Step 4: Run** `go test ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: boardingparty CLI (apply, run, runs, get)`.

### Task 10: End to end on the cluster

**Files:**
- Create: `docs/e2e.md` (the exact steps below, for re-running)

- [ ] **Step 1: Prepare a namespace** `bp-todomvc` with tiny's namespace setup (agent env Secret with a metered `ANTHROPIC_API_KEY`, default-deny, hostname allow-list = `api.anthropic.com`, `demo.playwright.dev`), then `kubectl apply -f deploy/director.yaml -n bp-todomvc` with the worker image from Task 3 (re-tagged from `spike`).
- [ ] **Step 2: Apply and run.** `boardingparty apply -f examples/todomvc.yaml -n bp-todomvc`, then `boardingparty run "<persona>" "<story>" --wait -n bp-todomvc`. Expected: outcome `done` within 10 minutes; the tiny Session `bp-run-…` no longer exists afterwards.
- [ ] **Step 3: Check evidence.** `boardingparty get <id> --out /tmp/bp-e2e`. Expected: a `.webm` that plays and shows the todo list with 3 items, 1 completed; a trace zip that opens in `npx playwright show-trace`.
- [ ] **Step 4: Check the failure path.** Edit the story goal to something impossible on that site ("Pay for your todos with a card"), run again. Expected: outcome `stuck` or `failed` with a reason, at least one bug recorded, video uploaded, Session deleted.
- [ ] **Step 5: Check the watchdog.** Run with `--run-timeout=1m` on the director and a goal requiring several minutes. Expected: run `failed`, outcome `no finish within 1m0s`, Session deleted.
- [ ] **Step 6: Commit** `docs: end-to-end check for plan 1`, and tag `v0.1.0`.
