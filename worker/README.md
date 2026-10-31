# boardingparty worker image

Image used by every boardingparty run. A run is one tiny Session (a Kubernetes pod);
tiny injects Claude Code, tmux and its entrypoint into this image through an init
container and overrides the command, so the Dockerfile sets no ENTRYPOINT or CMD.

Target name: `ghcr.io/boarding-party/worker:<tag>`. Local dev tag: `boardingparty-worker:dev`.

## Contents

- Base: `mcr.microsoft.com/playwright:v1.64.1-noble` (latest stable Playwright image; Ubuntu 24.04, glibc, node 24, Chromium/Firefox/WebKit).
- `git` (tiny's entrypoint refuses images without it), `curl`, `ca-certificates`.
- `@playwright/mcp@0.0.84` installed globally (`playwright-mcp` on PATH).
- Chromium revision 1252 (see "Version choice").
- `WORKDIR /workspace`. In a tiny session `/workspace` is a PVC and the agent runs as uid 1000.

Build: `docker build -t boardingparty-worker:dev worker/`

## Version choice

`@playwright/mcp` has no stable line: every release (0.0.73 to 0.0.84) depends on a
Playwright alpha of the next minor (0.0.84 needs `playwright 1.65.0-alpha-2026-10-30`).
The newest stable base image is `v1.64.1-noble`, whose Chromium is revision 1248, while
the alpha wants revision 1252. Without a fix every tool call fails with
`Browser "chrome-for-testing" is not installed; expected executable at /ms-playwright/chromium-1252/...`.
Passing `--executable-path` to the base's 1248 build also works but the path differs per
architecture (`chrome-linux` vs `chrome-linux-arm64`), so the Dockerfile instead runs
`playwright-mcp install-browser chrome-for-testing` once at build time into
`/ms-playwright` (the base's `PLAYWRIGHT_BROWSERS_PATH`). Cost: about 250 MB extra
(Chromium 1252 plus headless shell). The 1248 browsers remain as unused fallback.
When bumping `@playwright/mcp`, rebuild; the install step follows whatever revision it expects.

## Why `--no-sandbox`

tiny runs the agent as non-root uid 1000 with all capabilities dropped and seccomp
`RuntimeDefault`. Chromium's sandbox needs user namespaces or setuid helpers, which
that profile does not allow, so it is disabled. The pod itself is the isolation boundary.

## MCP server definition

Flag names confirmed with `playwright-mcp --help` (0.0.84):

| Purpose | Flag |
| --- | --- |
| headless | `--headless` |
| in-memory profile | `--isolated` |
| no Chromium sandbox | `--no-sandbox` |
| browser | `--browser chromium` |
| output dir | `--output-dir <path>` |
| viewport | `--viewport-size 1280x800` (also `--device "<name>"`, `--mobile`) |
| storage state | `--storage-state <path>` (isolated sessions) |
| enable video and tracing tools | `--caps=devtools` |
| config file | `--config <path>` |

`--save-video` and `--save-trace` DO NOT EXIST in 0.0.84 (the brief's guess was wrong).
Video and tracing are tools, available only with `--caps=devtools`:
`browser_start_video` (optional `filename`, `size`, `fps`, `cursor`), `browser_stop_video`,
`browser_start_tracing`, `browser_stop_tracing`. Files land in `--output-dir`
(`video-<timestamp>.webm`, `traces/trace-<ts>.trace` plus `screencast/` JPEGs). The agent
(or director prompt) must call start before and stop after the scenario, or the files are
not finalised.

```json
{
  "name": "pw",
  "command": "playwright-mcp",
  "args": ["--headless", "--isolated", "--no-sandbox", "--browser=chromium",
           "--output-dir=/workspace/out", "--viewport-size=1280x800", "--caps=devtools"]
}
```

## Verified (local docker, arm64, 2026-10-31)

All as `--user 1000 --cap-drop ALL --security-opt no-new-privileges` (Docker's default seccomp applies):

1. `playwright-mcp --help` lists the flags in the table above.
2. Real MCP stdio session with the args above (output dir `/tmp/out`): `browser_start_video`
   (1280x800), `browser_start_tracing`, `browser_navigate` to a `data:` URL, stop both.
   Chromium launched without sandbox or seccomp errors; produced a `.webm` (7.4 KB),
   a `.trace` with screencast frames, and a page snapshot `.yml`.
3. `git 2.43.0`, `curl 8.5.0`, `node v24.22.1`.

Not yet verified: running under tiny itself (needs push to ghcr and a cluster), and
linux/amd64 (local build was arm64; build the pushed image for the cluster's architecture).
