# boardingparty — design

Date: 2026-10-29
Status: draft for review

## 1. What it is and why

boardingparty runs a standing cast of AI "virtual users" against a website's
staging environment. Each virtual user (a persona) has a role, a login, an
email inbox, a personality and a history. Personas pursue plain-language
goals ("book a session for next week") through the real UI, hand work to each
other through the site itself (patient books → clinician approves → patient
joins → clinician writes outcome), and keep doing so for days and weeks.

Three things come out of it:

1. **Bugs with evidence.** Every story is recorded (video + Playwright trace).
   When a persona gets stuck or sees an error, a bug is filed with the video
   jumping to the moment it happened, the steps, the failing request and
   the persona's own note. Many personas hitting one problem make one bug.
2. **Realistic staging data.** Staging stops being "test test blablabla".
   Personas create believable, internally consistent records over time
   (a patient with six appointments, two cancellations and a mood trend),
   useful for humans who demo or test by hand.
3. **A second opinion from supervisors.** Supervisor roles use their own side
   of the site (oversight screens) and may optionally watch other personas'
   recordings to spot problems the user personas did not notice.

First two targets: augmentive.io (NHS mental-health appointments) and
LumiStack (marketing: products, websites, orders). The engine must know
nothing about either domain; all domain knowledge lives in the project's
roles and stories.

Distribution: open-source repo; the user installs it into their own
Kubernetes cluster. A commercial hosted version comes later and reuses the
same components (one director per customer).

## 2. Goals, non-goals, anti-goals

Goals
- Generic engine: a new site is configured with roles + stories in plain
  language, no code.
- Always on: runs in the cluster, independent of anyone's laptop. Long
  multi-role chains survive restarts and multi-hour waits.
- Controllable cost and load: cast size, activity per role, actions per
  hour, spend per day, data caps, pause/stop.
- Every failure is reproducible from its recording.
- Runs on the user's own cluster with nothing exposed to the internet.

Non-goals (v1)
- GitHub/GitLab/Jira integration. Bugs live in boardingparty; export is a
  download.
- Generating Playwright regression tests from runs.
- Load/performance testing. The rate limit is there to keep load *low*.
- Hosted/commercial version, billing, multi-tenant anything.
- Testing native mobile apps (mobile = phone-sized browser viewport only).

Anti-goals (would make it a failure even if it works)
- Ever reaching production. Agents may only open the configured staging
  hosts plus the built-in inbox; nothing else is reachable.
- Real personal data. All identity data is synthetic (NHS numbers from the
  reserved test range, fake names, real-format postcodes).
- Silent spend. Hitting the daily budget pauses the cast and says so.
- One pod per persona. Cost must scale with concurrency, not cast size.
- Changes to tiny that only make sense for boardingparty. A tiny change is
  allowed when it is generic and useful to any tiny user (§4.3).

## 3. Concepts

- **Project**: one site under test. Holds URLs, roles, limits, cluster
  context and namespace.
- **Role**: description, sign-in mode (self sign-up with own inbox, or
  pre-made accounts), people count, activity pattern, behaviour mix,
  stories, and `canWatchRecordings` (makes it a supervisor).
- **Persona**: one virtual user of a role. Data, not a process: identity,
  traits, credentials, browser storage state (cookies), notes/memory,
  history, inbox address.
- **Story**: a goal in plain words, never click steps. May declare
  *preconditions* ("an approved session starting within 10 min") and a
  *handoff* ("then waits for: Clinician approves, up to 8 h").
- **Handoff**: a dependency between personas, satisfied by something
  observable on the site or in email, not by personas talking to each
  other. The director tracks it; personas never message each other.
- **Assignment**: "persona X, story Y, now". The unit of work given to a
  worker.
- **Run**: one executed assignment, with outcome (done / stuck / failed /
  flagged), recording, trace, log and cost.
- **Bug**: a grouped problem. Fingerprint from failing step + error/request
  signature + page; hits accumulate on the same bug.

## 4. Architecture

```
laptop                                client cluster, namespace bp-<project>
┌───────────────────┐  kube API     ┌──────────────────────────────────────────┐
│ boardingparty CLI │ (port-forward │ director  (Deployment, 1 replica, Go)    │
│  serves web UI on ├──under user's─▶│  SQLite on PVC                           │
│  localhost        │    RBAC)      │  scheduler · handoffs · limits · budget  │
└───────────────────┘               │  HTTP API (UI)  ·  MCP server (workers)  │
                                    │                                          │
                                    │ workers: tiny Sessions (N, default 4)    │
                                    │  image: Playwright + Chromium + tiny     │
                                    │  payload; Claude Code drives the browser │
                                    │                                          │
                                    │ Mailpit (all persona inboxes)            │
                                    │ MinIO / S3 (videos, traces, screenshots) │
                                    └───────────────┬──────────────────────────┘
                                                    ▼ egress allow-list only
                                             staging site(s)
```

### 4.1 Director
- Single Go binary, one replica, state in SQLite on a PVC. One pod keeps
  the open-source install small; the hosted version later runs one
  director per customer, so no multi-tenant database is needed.
- No Service exposed outside the namespace, no Ingress. The UI reaches it
  via the Kubernetes API (port-forward) under the user's own RBAC, the same
  access model as tiny.
- Responsibilities: store config; generate and hold personas; scheduler;
  handoff tracking; limit and budget enforcement; bug grouping; recording
  index; retention; HTTP API for the UI; MCP server for workers.

### 4.2 Workers (tiny sessions)
- The director creates tiny `Session` objects
  (`agents.tinysystems.io/v1alpha1`) via the Kubernetes API, with
  `spec.image` set to the boardingparty worker image (glibc, Playwright +
  Chromium; tiny injects its payload). Verified in tiny source: `spec.image`
  accepts any glibc image, `spec.task` sets the starting prompt,
  `spec.inbox` delivers further messages to a running session.
- One tiny Session per run (builder's call, recorded): the director
  creates a Session for an assignment and deletes it when the run
  finishes. This keeps personas isolated (no shared transcript, no context
  growth) at the cost of pod start-up time, which is acceptable at a few
  actions per persona per day. N (default 4) is the maximum number of run
  Sessions alive at once. Inside a run: `next_assignment` → load the persona
  (storage state, identity, notes, traits) → pursue the story in the
  browser with recording on → upload video and trace → `finish_run` /
  `report_bug`.
- Personas never own a pod; all persona state lives in the director.
- Chromium runs with `--no-sandbox` because tiny's restricted container
  profile drops every capability and blocks user namespaces. The pod is the
  sandbox. Must be verified early.
- Behaviour traits (impatient, typos, screen reader, phone viewport) are
  applied by the worker: viewport/device emulation, typing style
  instructions, accessibility-tree navigation for screen-reader personas.

### 4.3 Worker ↔ director: MCP, via a small tiny feature
tiny's controller starts every session with `/tiny/entrypoint.sh`
(internal/workload/workload.go), which seeds only the `tiny` MCP server.
Decision (user, 2026-10-29: tiny may change where it fits tiny): add a
generic `spec.mcpServers` to tiny's `Session` (name → HTTP URL, optional
header from a Secret key), written by the entrypoint next to the `tiny`
server for both claude and codex. It fits tiny on its own: any session may
need an in-cluster MCP server. Lands in tiny first, as its own release.

Director MCP tools (the run token in the header identifies persona + run):
- `next_assignment()` → persona + story + context for this run.
- `persona_save(state)` → storage state, notes, data created.
- `inbox_list()` / `inbox_read(id)` → the persona's Mailpit inbox.
- `finish_run(outcome, summary)`.
- `report_bug(summary, step, evidence, severity_guess, note)`.
- Supervisor-only: `list_recordings(filter)`, `get_recording(id)` (sampled
  frames + trace summary), `flag_run(id, problem)`. The director checks the
  role behind the run token; user-role runs get "not allowed".

### 4.4 Email
- Mailpit in the namespace. Personas get addresses on one domain
  (`<slug>@inbox.<project>`); the staging site must be configured to send
  mail via Mailpit's SMTP. If the site can't be pointed at a custom SMTP,
  that site can't use self sign-up. Documented as a precondition.

### 4.5 Recordings
- Playwright video + trace per run, uploaded to S3-compatible storage
  (tiny's MinIO add-on, or any S3 via settings). Director stores the index
  only.
- Retention: successful runs 3 days, failed/stuck runs until their bug is
  marked fixed + 7 days. Configurable.

### 4.6 CLI and web UI
- `boardingparty` CLI (Go). `boardingparty ui` serves the web UI on
  localhost and port-forwards to the director of the selected project.
  `boardingparty install` requires the tiny controller already on the
  cluster (checked, with a clear message if missing). It creates the
  namespace, sets it up for tiny through tiny's own existing mechanisms
  (service account, agent env secret holding the API key, default-deny
  policy, hostname allow-list), then installs director, Mailpit, worker
  RBAC (create/delete Sessions in this namespace only). Also `start`, `stop`,
  `status`, `report` for scripting.
- UI is static assets embedded in the CLI binary. Pages per the approved
  prototype (`docs/superpowers/specs/2026-10-29-boardingparty-prototype.html`):
  Projects, New project (cluster → site → model and budget → install),
  Overview, Cast (handoff swimlanes + list), Persona (timeline, inbox, data
  created, recordings), Roles + role editor, Bugs + bug detail (video with
  error marks, steps, request, persona note, prev/next hit), Recordings,
  Staging data, Limits & settings. A run strip on every page: state,
  actions/hour vs limit, spend vs budget, data vs cap, Pause, Stop all.

## 5. Scheduling and limits

- Each role has an activity pattern (e.g. "2–4 actions a day, 07:00–23:00";
  "every 30 min, 09:00–17:30"; "daily at 09:00"). The director turns these
  into due times per persona with jitter.
- An assignment is eligible when the persona is due, story preconditions
  hold, and any handoff it waits for is satisfied or timed out (timeout =
  the story continues and notes "never approved", which is a finding in
  itself).
- Global token bucket for actions per hour; daily spend counter from the
  model usage that workers report; worker pool size caps concurrency.
- Data caps per record type. At the cap, creating stories are skipped and
  personas use edit/cancel/reuse stories. "Delete everything the cast
  created" is a UI action: personas delete their own data through the site
  where possible; otherwise it is listed for the user.
- Pause = no new assignments, running ones finish. Stop = workers deleted.

### 5.1 Nothing to do costs nothing
Tokens are spent only inside a run, and a run starts only when there is
something the persona can actually do. Deciding that is the director's job
and uses no model calls.

Persona states (shown on Cast):
- **working**: a run is in progress.
- **idle**: no story is due (outside activity hours, or just acted).
- **waiting**: a run ended because the persona depends on something not
  there yet. The run declares what it waits for as a machine-checkable
  condition: an email matching a subject/sender in its inbox, a handoff
  completed by another role, or a time. The director checks these cheaply
  (Mailpit API, its own handoff table, the clock) and schedules the next run
  only when the condition holds.
- **stuck**: a wait passed its timeout, or a run hit something it cannot get
  past. Filed as a finding; the persona stops trying that story until a
  human nudges it or the related bug is marked fixed.

A run that finds nothing to do ends at once with `finish_run(waiting, until=…)`;
agents never poll a page or an inbox in a loop.

### 5.2 Access gates belong to roles, not to the engine
Sites differ in how people get in: open sign-up, email confirmation, admin
approval, invitations. The engine has no special case for any of them. A gate
that a human would clear inside the site (approve a new account, send an
invite) is a story of an admin-type role, and the user persona waits on it as
a handoff. A gate outside the site (a real identity check against an
external service) can't be cleared by the cast; those roles use pre-made
accounts. Email works for any site whose staging can send mail through
Mailpit's SMTP; a site that can't uses pre-made accounts and skips stories
that need email.

## 6. Open questions and risks

1. Resolved: worker ↔ director uses MCP through a new generic
   `spec.mcpServers` in tiny (§4.3).
2. **Chromium under tiny's restricted profile.** `--no-sandbox` expected to
   work; verify with a spike before building the scheduler.
3. **Model credentials and budget.** Automated use requires a metered API
   key, not a Pro/Max subscription. Key stored as tiny's agent env Secret.
   The daily budget is a soft cap (builder's call, recorded): runs report
   cost on finish; once over, no new runs start and running ones finish.
   Overspend is bounded by N × one run.
4. **Egress.** Allow-list = the model API + staging hosts + a per-project
   "extra hosts" list (identity provider, video-call provider, payment
   sandbox, CDNs) shown in Limits & settings. tiny's `egress` refusals log
   is read by the director; a refused host during a run is reported as a
   finding ("story needs host X") rather than a site bug. In-namespace
   Mailpit, director and MinIO are reachable by default policy; verify the
   proxy doesn't break in-namespace HTTP.
5. **Bug grouping quality.** Fingerprinting by step + error signature may
   over- or under-group; v1 lets the user merge/split bugs by hand.
6. **Name.** "boardingparty" is the project/folder name; the user may still
   rename the product.

## 7. Left to the builder

- Exact SQLite schema, migrations, and API shape.
- UI framework for the embedded web app (keep it light; compact
  functional UI as in the prototype).
- Synthetic data generation approach (LLM-written within rules + format
  validators such as the NHS number checksum).
- Packaging of the in-cluster pieces (Helm chart vs manifests applied by
  the CLI).
