# End-to-end check (Plan 1)

Run these steps against a real cluster with tiny installed. Plan 1 runs one
run at a time; the director answers 409 while another run is active.

1. Prepare the namespace `bp-todomvc` with tiny's namespace setup:
   - an agent env Secret holding a metered `ANTHROPIC_API_KEY`;
   - default-deny network policy;
   - hostname allow-list: `api.anthropic.com`, `demo.playwright.dev`.

   The director installs itself on the first command that needs it (step 2).
   To do it earlier, or to review the manifest with `--dry-run`, run
   `boardingparty setup -n bp-todomvc`. Either way it applies
   `deploy/director.yaml` with server-side apply and pulls
   `ghcr.io/boarding-party/director` and `ghcr.io/boarding-party/worker` (built
   by CI; the tag follows the CLI's release version, else `main`; override it
   with `setup --image-tag`).

   The director passes workers `http://bp-director.<namespace>.svc:8080`. It
   must be a `.svc` name: tiny's egress proxy only bypasses `.svc` and
   `.cluster.local` hosts.

   Chromium's use of the egress proxy must be checked. If pages fail to load,
   add `--proxy-server=http://tiny-egress:3128` to the `pw` server args.

2. Apply and run:

   ```
   boardingparty apply -f examples/todomvc.yaml -n bp-todomvc
   boardingparty run "<persona>" "<story>" --wait -n bp-todomvc
   ```

   Expected: outcome `done` within 10 minutes; the tiny Session `bp-run-...`
   no longer exists afterwards.

3. Check evidence:

   ```
   boardingparty get <id> --out /tmp/bp-e2e
   ```

   Expected: a `.webm` that plays and shows the todo list with 3 items, 1
   completed; a trace zip that opens in `npx playwright show-trace`.

4. Check the failure path. Edit the story goal to something impossible on that
   site ("Pay for your todos with a card"), apply, and run again. Expected:
   outcome `stuck` or `failed` with a reason, at least one bug recorded, video
   uploaded, Session deleted.

5. Check the watchdog. Run with `--run-timeout=1m` on the director and a goal
   that needs several minutes. Expected: run `failed`, outcome
   `no finish within 1m0s`, Session deleted.
