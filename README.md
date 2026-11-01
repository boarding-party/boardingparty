# boardingparty

A cast of AI users for your staging site. Each one has a role, a login, a
personality and a goal ("book a session for next week"). They use the site
through a real browser, hand work to each other the way real users do, and
record every visit. When one gets stuck or sees an error, you get a bug with
the video cued to the moment it happened.

It runs on your own Kubernetes cluster, on top of
[tiny](https://github.com/tiny-systems/tiny). Nothing is exposed to the
internet, and agents can only reach the hosts you allow.

> Status: early. One persona, one story, end to end is built and tested in
> code, along with `boardingparty board` (a local web UI with runs, run
> detail with video, and a project editor) and `boardingparty leave`. It has
> not yet run on a real cluster. Scheduling, handoffs, email, profiles,
> supervisors and limits come next; see [Roadmap](#roadmap).

## What a run looks like

1. You describe the site in a project file: staging URLs, roles, personas
   and stories. Stories are goals in plain words, never click-by-click
   steps.
2. You start a run: `boardingparty run "Aisha Khan" "book-session"`.
3. The director creates one tiny session (a pod) for that run. Inside it,
   Claude Code drives Chromium through Playwright.
4. The agent asks the director who it is and what to do, then works through
   the site as that persona. It records video and a trace the whole time.
5. When it finishes or gets stuck, it uploads the recordings, files any bugs
   it saw, saves what the persona should remember, and reports an outcome:
   `done`, `stuck` or `failed`.
6. The director deletes the pod. You read the result with
   `boardingparty get <run-id>`, which downloads the video and trace.

If an agent never reports back, a watchdog ends the run after 20 minutes
and deletes its pod.

## Architecture

```
your laptop                          your cluster, namespace bp-<project>
┌────────────────────┐             ┌────────────────────────────────────────────┐
│ boardingparty CLI  │  kubectl    │ director  (1 pod, Go, SQLite on a volume)  │
│                    ├─port-forward┤  admin API     127.0.0.1:8081  ◄── you     │
│                    │  (your RBAC)│  worker API    :8080           ◄── agents  │
└────────────────────┘             │                                            │
                                   │ run session  (tiny pod, one per run)       │
                                   │  Claude Code ── Playwright MCP ── Chromium │
                                   │       │                                    │
                                   │       └── MCP + uploads ──► director :8080 │
                                   │                                            │
                                   │ tiny-egress  (allow-list proxy)            │
                                   └──────────────────┬─────────────────────────┘
                                                      ▼  allowed hosts only
                                     staging site, model API
```

### The pieces

**Director** (`cmd/director`). One pod with one SQLite database on a
persistent volume. It stores the project, runs, bugs, persona state and
recordings, starts and stops run sessions, and runs the watchdog. It listens
on two addresses on purpose:

- `127.0.0.1:8081`, the admin API. It is bound to the pod's loopback, so
  nothing else in the cluster can reach it. You reach it with a
  port-forward, which needs your own Kubernetes permissions.
- `:8080`, the worker API. Agents use it, and it serves only three things:
  the MCP endpoint at `/mcp/run/<token>`, recording uploads at
  `/artifacts/<token>/<name>`, and `/healthz`.

**Run sessions.** Each run is a tiny session with the worker image
(`worker/`). That image is Playwright's Ubuntu image plus Playwright MCP and
a newer Chromium. tiny adds Claude Code and starts it with the run's
instructions and two MCP servers:

- `pw`: Playwright MCP, which drives the browser.
- `bp`: the director, which tells the agent who it is and takes its reports.

The agent gets these tools from the director:

| tool | what it does |
|---|---|
| `next_assignment` | persona (name, role, traits, synthetic identity), story goal, allowed sites, saved persona state, upload URL |
| `persona_save` | stores what the persona should remember next time |
| `report_bug` | summary, step, evidence, severity guess, the persona's own words |
| `finish_run` | `done`, `stuck` or `failed`, with a summary |

A persona is just rows in the director's database. A pod exists only while a
run is in progress, so fifty personas cost nothing until one of them acts.

**CLI** (`cmd/boardingparty`). `board`, `setup`, `apply`, `run`, `runs`, `get`
and `leave`. Every command except `leave` opens a port-forward to the
director's admin API and closes it when done; `board` keeps it open while the
UI is in use. `leave` talks to the Kubernetes API directly.

## Security model

Agents read untrusted web pages, so the design assumes an agent can be
talked into doing the wrong thing.

- tiny's default-deny policy and hostname allow-list limit an agent to the
  staging hosts and the model API you list. If you point a project at
  production, the pages won't load unless you also allow production hosts.
  Don't.
- Agents can't reach the admin API at all, because it isn't on any network
  they can see. Through the worker port they can only act on their own run.
- Each run gets its own token, carried in the agent's MCP URL. It names one
  run and one persona, can't read or write another run's data, and stops
  working when the run finishes or times out.
- For now only one run happens at a time. tiny session pods can read the session
  objects in their namespace, and the token sits in the session spec. So the
  director refuses to start a run while any run session or its pod still
  exists, and it deletes sessions in the foreground so that state lasts until
  the pods are gone. Parallel runs need the token moved out of the spec
  first (planned).
- Bug text and run outcomes are written by agents, so the CLI strips terminal
  control and formatting characters from anything the server sends before
  printing it.
- Personas use made-up identities. Never give them real accounts or real
  people's details.
- The admin API wants a bearer key on every request. `setup` (or first use)
  generates it once into the Secret `bp-director-admin`, and the CLI reads it
  with your kube credentials. The key is never printed. A port-forward is a
  plain TCP port on your machine, so the key also stops other local programs
  and web pages from using the forward. The director also refuses requests
  with a non-loopback `Host`, with browser `Origin` or `Sec-Fetch-Site`
  headers, or with form and `text/plain` bodies.
- `boardingparty board` listens on 127.0.0.1 only. The link it prints holds
  a key that works once and expires after 10 minutes. Opening it swaps the
  key for a separate session cookie (HttpOnly, SameSite=Strict). Requests
  without that cookie get 401, and requests with a `Host` other than
  `127.0.0.1:<port>` or `localhost:<port>` get 403.

Browsers don't scope cookies by port, so while `board` runs, other local
servers on 127.0.0.1 (a dev server, say) receive the board session cookie
with every request your browser sends them. Someone who can read those
servers' logs could use the board until you stop it. The cookie stops working
when `board` exits.

Two things this doesn't protect. The model API key sits in the run pod,
because Claude Code needs it. And anyone who can read Secrets and port-forward
in the namespace has full admin access.

## What gets installed

All of it in one namespace per project:

| object | why |
|---|---|
| Namespace `bp-<project>` | everything below lives here |
| tiny's base: service account `tiny-session`, its Role and RoleBinding | what run pods run as |
| tiny's egress add-on: default-deny NetworkPolicy, `tiny-egress` proxy, allow-list | the network fence |
| Secret `tiny-agent-env` with `ANTHROPIC_API_KEY` | the key run pods use |
| Director: service account, Role, RoleBinding, 5Gi volume, Deployment, ClusterIP Service | `deploy/director.yaml`, installed on first use or by `boardingparty setup` |
| LimitRange `bp-defaults` and ResourceQuota `bp-quota` | resource limits for the namespace, applied before the Deployment |
| Secret `bp-director-admin` | the admin API key, created once by setup and never rotated |

There's no Ingress, no public address and no cluster-wide role. tiny's
`Session` CRD is cluster-wide and is installed once per cluster.

Nothing in the namespace runs unbounded. The LimitRange gives any container
without limits a default of 1 CPU and 1Gi of memory (requests 100m and 256Mi),
and caps a single container at 4 CPU and 8Gi. That covers tiny's sidecar and
init containers. The quota caps the namespace at 12Gi of memory limits, 8 CPU
of limits, 4 CPU of requests and 12 pods. The agent container of a run pod is
sized from the project's `resources` (default `cpu: "1"`, `memory: "3Gi"`;
CPU between 250m and 4, memory between 1Gi and 8Gi). Memory is also its
limit, so a runaway build is killed in its own pod, not the node. A CPU
request above 1 needs a matching limit in the LimitRange, so edit `bp-defaults`
if you raise `resources.cpu`.

The director's Role is limited to its own namespace: sessions
(get/list/watch/create/delete), persistent volume claims and deployments
(get/create, which tiny needs to build a session), configmaps and services
(get), and pods (list, for the one-run-at-a-time check).

## Requirements

- A dedicated node pool for run pods when the cluster also runs production
  workloads. Limits stop one run starving a node, but a browser and a coding
  agent are heavy neighbours. Pinning run pods to a pool with a nodeSelector
  and toleration is future work.
- Kubernetes with a CNI that enforces NetworkPolicy. Without that, the
  allow-list is only advisory.
- tiny v0.11.0 or later. Earlier versions lack `spec.mcpServers` and the
  `pkg/session` package the director uses.
- A metered Anthropic API key. Runs are automated, so a Claude Pro or Max
  subscription isn't the right fit under the provider's terms.
- Images: `ghcr.io/boarding-party/director` and `ghcr.io/boarding-party/worker`,
  multi-arch, built by CI on every push to `main`.

## Trying it

Before you start:

1. Run `tiny init -n bp-todomvc` to set up tiny in the namespace.
2. Put your metered Anthropic key in tiny's agent secret (`tiny-agent-env`).

That's all you install by hand. The CLI installs the director on first use,
waits for it, and upgrades it in place when you get a newer CLI. If tiny isn't
set up, it stops and tells you to run `tiny init`; it never changes anything
tiny owns.

Then open the board:

```
boardingparty board
```

In a terminal it asks which kubeconfig context and namespace to use (see
below), installs the director if needed, and prints a link to a local page on
127.0.0.1. The link works once and expires after 10 minutes; press Enter for a
new one. From the page you edit the project, start a run, and watch it finish.
A finished run shows its recording with the bugs listed beside it, and a bug
with a time in its step jumps the video to that moment. Ctrl-C stops the board.

`examples/todomvc.yaml` points one persona at Playwright's public TodoMVC
demo; paste it into the project editor to start. [docs/e2e.md](docs/e2e.md)
has the full check: a successful run, a run that gets stuck on purpose, and a
watchdog timeout.

Every command works on one cluster and namespace. Leave out `--context` and
`-n` in a terminal and the CLI asks: first the kubeconfig context (your
current one is preselected), then the namespace, with the ones where tiny is
set up listed first. It remembers your last choice and prints
`target: <context> / <namespace>` on every command so you can see where it is
acting. Esc cancels without changing anything. In CI, with no terminal, it
never asks: pass `-n <namespace>` (and `--context` if you don't want the
current one).

A run takes a few minutes, mostly pod start-up and the agent's own pace. Cost
depends on the model and how long the agent works: from a few pence to around
a pound per run for a short story.

### Scripting and CI

Everything the board does is also a command:

```
boardingparty setup -n bp-todomvc              # install the director; --dry-run shows the manifest
boardingparty apply -f examples/todomvc.yaml -n bp-todomvc
boardingparty run "Ada" "todos" --wait -n bp-todomvc
boardingparty runs -n bp-todomvc
boardingparty get <run-id> --out ./evidence -n bp-todomvc
```

`setup` is the install step on its own, for when you want to review it first.
`get` downloads the recording and the trace.

### Cleaning up

```
boardingparty leave -n bp-todomvc
```

This stops any active run and removes what boardingparty installed: the
director, its service, role and admin secret, and its data volume. It leaves
tiny's own objects and the namespace alone. Use `--keep-data` to keep the
volume (and so your runs and recordings) for a later install. It asks before
deleting; without a terminal you must pass `--yes`.

## Developing

`go vet ./... && go test -race ./...` runs the tests. To work on the board UI
without a cluster, run this from the repository root:

```
go run ./cmd/boardingparty board --dev-director --no-open
```

It prints `dev director: no cluster, fake runs` and a link. The director is
in-process, with the todomvc project loaded and the same admin key check as
the real one. A started run goes to running and, after 20 seconds, finishes
with a sample recording and one bug at 00:02. The flag is hidden from `--help`
because it is only for this.

## Roadmap

**Plan 2: a living cast.**
- A scheduler with activity patterns per role.
- Handoffs between roles, such as patient books, then clinician approves,
  then patient joins.
- `waiting` and `stuck` states that cost nothing while idle.
- Limits: actions per hour, daily spend, data caps.
- Bug grouping.
- Supervisor roles that review other personas' recordings.
- A persona workspace: email via IMAP, files, calendar, a password vault with
  2FA codes, payment test cards.
- Structured persona profiles: age, computer confidence, vision, reading,
  device, assistive technology.

**Already here.**
- `boardingparty setup` installs the director; `leave` removes it.
- `boardingparty board` serves a local web UI: runs, run detail with video,
  and a project editor.

**Later.**
- UI screens for the cast, a persona timeline and limits.

Design: [docs/superpowers/specs/2026-10-29-boardingparty-design.md](docs/superpowers/specs/2026-10-29-boardingparty-design.md).

## Repository layout

```
cmd/director         the in-cluster service
cmd/boardingparty    the CLI
internal/api         admin and worker HTTP handlers
internal/mcpserver   the tools agents call
internal/runs        run lifecycle, one-at-a-time gate, watchdog, reaper
internal/launcher    run sessions via tiny pkg/session; the agent's instructions
internal/store       SQLite
internal/artifacts   recording storage
internal/project     project file format
internal/cli         CLI commands and port-forward
internal/board       the local UI server, proxy and static UI
internal/devdirector fake in-process director for `board --dev-director`
worker/              the run image
deploy/              Kubernetes manifests
```

## Licence

MIT.
