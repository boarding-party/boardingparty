// boardingparty board: runs list, start run, run detail.
//
// Everything the director sends is untrusted (bug text is agent-written), so
// it is only ever rendered as text through htm/Preact, which escapes it.
// Never use innerHTML or dangerouslySetInnerHTML here.
import { render } from './vendor/preact.module.js';
import { useState, useEffect, useRef, useCallback } from './vendor/hooks.module.js';
import { html, api, directorDown } from './lib.js';

const ACTIVE = new Set(['pending', 'running']);
const STATE_TAG = new Map([['pending', 'idle'], ['running', 'run'], ['done', 'ok'], ['stuck', 'wait'], ['failed', 'bad']]);
const RETRY_MS = [5000, 10000, 30000]; // back-off after consecutive errors
const MAX_FINISHED_FAILURES = 5;
const ACTIVE_POLL_MS = 5000;
const IDLE_POLL_MS = 30000;

// ---- helpers ------------------------------------------------------------

function parseTime(s) {
  if (!s) return null;
  const d = new Date(s);
  // Go's zero time (0001-01-01) means "not set".
  return isNaN(d) || d.getUTCFullYear() < 2000 ? null : d;
}

const sameDay = (a, b) => a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();

function fmtWhen(s) {
  const d = parseTime(s);
  if (!d) return '';
  const time = d.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' });
  if (sameDay(d, new Date())) return time;
  return d.toLocaleDateString('en-GB', { day: 'numeric', month: 'short' }) + ' ' + time;
}

function fmtFull(s) {
  const d = parseTime(s);
  return d ? d.toLocaleString('en-GB', { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '';
}

function fmtDuration(ms) {
  if (ms < 0 || !isFinite(ms)) return '';
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} m ${s % 60} s`;
  return `${Math.floor(m / 60)} h ${m % 60} m`;
}

const firstLine = (s) => (s || '').split('\n').find((l) => l.trim()) || '';

// stampOf finds the first m:ss, mm:ss or h:mm:ss in a bug's text and returns
// it in seconds, or null. The step is checked first: that is where the agent
// is asked to say when it happened.
const STAMP = /(?<![\d:])(?:(\d{1,2}):)?(\d{1,2}):([0-5]\d)(?![\d:])/;
function stampOf(bug) {
  for (const t of [bug.step, bug.summary, bug.evidence, bug.note]) {
    const m = STAMP.exec(t || '');
    if (m) return (Number(m[1] || 0) * 3600) + Number(m[2]) * 60 + Number(m[3]);
  }
  return null;
}

function fmtStamp(secs) {
  const h = Math.floor(secs / 3600), m = Math.floor((secs % 3600) / 60), s = secs % 60;
  const mm = String(m).padStart(2, '0'), ss = String(s).padStart(2, '0');
  return h ? `${h}:${mm}:${ss}` : `${mm}:${ss}`;
}

const artifactURL = (runID, name) => `/api/runs/${encodeURIComponent(runID)}/artifacts/${encodeURIComponent(name)}`;
const runHref = (id) => `#/runs/${encodeURIComponent(id)}`;

function allPersonas(project) {
  const out = [];
  for (const r of (project && project.roles) || []) {
    for (const p of r.personas || []) out.push({ name: p.name, role: r.name });
  }
  return out;
}

function storiesFor(project, personaName) {
  for (const r of (project && project.roles) || []) {
    if ((r.personas || []).some((p) => p.name === personaName)) return r.stories || [];
  }
  return [];
}

// ---- routing ------------------------------------------------------------

function parseRoute(hash) {
  const parts = (hash || '').replace(/^#\/?/, '').split('/');
  if (parts[0] === 'project') return { page: 'project' };
  if (parts[0] === 'runs' && parts[1]) {
    try { return { page: 'run', id: decodeURIComponent(parts[1]) }; } catch (_) { /* fall through */ }
  }
  return { page: 'runs' };
}

function useRoute() {
  const [route, setRoute] = useState(() => parseRoute(location.hash));
  useEffect(() => {
    const on = () => { setRoute(parseRoute(location.hash)); window.scrollTo(0, 0); };
    window.addEventListener('hashchange', on);
    return () => window.removeEventListener('hashchange', on);
  }, []);
  return route;
}

// usePoll calls fn now, then again after however many ms fn returns, until
// fn returns null or the component unmounts or deps change. fn gets a
// stale() check: after each await it must bail out (return null) if stale()
// is true, so a late response never lands in an unmounted or re-keyed view.
function usePoll(fn, deps) {
  useEffect(() => {
    let stop = false, timer;
    const stale = () => stop;
    const tick = async () => {
      const next = await fn(stale);
      if (!stop && next != null) timer = setTimeout(tick, next);
    };
    tick();
    return () => { stop = true; clearTimeout(timer); };
  }, deps);
}

// ---- shell --------------------------------------------------------------

function App() {
  const route = useRoute();
  const [project, setProject] = useState(undefined); // undefined loading, null none
  const [projectErr, setProjectErr] = useState('');
  const [runs, setRuns] = useState(null);
  const [runsErr, setRunsErr] = useState('');
  const [director, setDirector] = useState('unknown'); // unknown | ok | down

  const loadProject = useCallback(async () => {
    try {
      setProject(await api('/api/project'));
      setProjectErr('');
    } catch (e) {
      if (e.status === 404) { setProject(null); setProjectErr(''); return; }
      setProjectErr(e.message);
    }
  }, []);

  const loadRuns = useCallback(async () => {
    try {
      const rs = (await api('/api/runs')) || [];
      setRuns(rs);
      setRunsErr('');
      setDirector('ok');
      return rs;
    } catch (e) {
      setRunsErr(e.message);
      setDirector(directorDown(e) ? 'down' : 'ok');
      return null;
    }
  }, []);

  usePoll(async (stale) => {
    const rs = await loadRuns();
    if (stale()) return null;
    return rs && rs.some((r) => ACTIVE.has(r.state)) ? ACTIVE_POLL_MS : IDLE_POLL_MS;
  }, [loadRuns]);

  useEffect(() => { loadProject(); }, [loadProject, route.page]);

  const active = (runs || []).find((r) => ACTIVE.has(r.state));

  return html`
    <div class="app">
      <${Sidebar} route=${route} project=${project} runs=${runs} />
      <main>
        <${RunStrip} director=${director} active=${active} />
        ${route.page === 'run' ? html`<${RunPage} key=${route.id} id=${route.id} onChange=${loadRuns} />`
          : route.page === 'project' ? html`<${ProjectPage} project=${project} error=${projectErr} />`
          : html`<${RunsPage} project=${project} projectErr=${projectErr} runs=${runs} runsErr=${runsErr}
                    active=${active} onStarted=${loadRuns} />`}
      </main>
    </div>`;
}

function Sidebar({ route, project, runs }) {
  const on = (p) => (route.page === p || (p === 'runs' && route.page === 'run') ? 'on' : '');
  const site = (project && project.sites && project.sites[0]) || '';
  let host = site;
  try { host = site ? new URL(site).host : ''; } catch (_) { /* show it as written */ }
  return html`
    <aside>
      <div class="brand">boardingparty</div>
      ${project ? html`<div class="proj">${project.name || 'Unnamed project'}<small>${host || 'no site yet'}</small></div>`
        : project === null ? html`<div class="proj">No project yet<small><a href="#/project" style="color:inherit">Set one up</a></small></div>`
        : null}
      <nav aria-label="Main">
        <a href="#/runs" class=${on('runs')}>Runs ${runs ? html`<span class="count">${runs.length}</span>` : null}</a>
        <a href="#/project" class=${on('project')}>Project</a>
      </nav>
      <div class="foot">Local board. Close the terminal to stop it.</div>
    </aside>`;
}

function RunStrip({ director, active }) {
  const state = director === 'ok' ? { dot: 'ok', label: 'Director ready' }
    : director === 'down' ? { dot: 'bad', label: 'Director unreachable' }
    : { dot: '', label: 'Connecting…' };
  return html`
    <div class="runbar">
      <div class="runstate"><span class=${'dot ' + state.dot}></span>${state.label}</div>
      ${active
        ? html`<div class="stat"><span class="dot live" style="display:inline-block;margin-right:6px"></span>
            <a href=${runHref(active.id)}><b>${active.persona}</b></a> is on <b>${active.story}</b>
            <span class="muted"> · ${active.state} since ${fmtWhen(active.started)}</span></div>`
        : director === 'ok' ? html`<div class="stat">No run active</div>` : null}
      <div class="grow"></div>
      <a class="btn" href="#/project">Edit project</a>
    </div>`;
}

// ---- runs page ----------------------------------------------------------

function RunsPage({ project, projectErr, runs, runsErr, active, onStarted }) {
  return html`
    <section class="page">
      <h1>Runs</h1>
      <p class="lede">One persona working through one story on your staging site. Runs go one at a time.</p>
      <div class="stack">
        ${projectErr ? html`<div class="note bad">Could not load the project: ${projectErr}</div>` : null}
        ${project === null
          ? html`<div class="empty"><b>Set up your project first</b>Runs need a site, a role, a persona and a story.
              <div style="margin-top:12px"><a class="btn primary" href="#/project">Set up your project</a></div></div>`
          : project ? html`<${StartRun} project=${project} active=${active} onStarted=${onStarted} />` : null}
        ${runsErr ? html`<div class="note bad">Could not load runs: ${runsErr}</div>` : null}
        ${runs && runs.length === 0 && project !== null
          ? html`<div class="empty"><b>No runs yet.</b>Start one above.</div>` : null}
        ${runs && runs.length > 0 ? html`<${RunsTable} runs=${runs} />` : null}
      </div>
    </section>`;
}

function StartRun({ project, active, onStarted }) {
  const personas = allPersonas(project);
  const [persona, setPersona] = useState('');
  const [story, setStory] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  // Keep the selection valid when the project changes.
  const personaOK = personas.some((p) => p.name === persona);
  const p = personaOK ? persona : (personas[0] ? personas[0].name : '');
  const stories = storiesFor(project, p);
  const s = stories.some((x) => x.name === story) ? story : (stories[0] ? stories[0].name : '');
  const goal = (stories.find((x) => x.name === s) || {}).goal;

  const roles = (project.roles || []).filter((r) => (r.personas || []).length);
  const disabled = !!active || busy || !p || !s;

  const submit = async (e) => {
    e.preventDefault();
    if (disabled) return;
    setBusy(true); setErr('');
    try {
      const run = await api('/api/runs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ persona: p, story: s }),
      });
      await onStarted();
      if (run && run.id) location.hash = runHref(run.id);
    } catch (e2) {
      setErr(e2.message); // 409 busy and 400 invalid are shown verbatim
      onStarted();
    } finally {
      setBusy(false);
    }
  };

  if (!personas.length) {
    return html`<div class="empty"><b>Add a persona to start a run</b>
      Your project has no personas yet. <a href="#/project">Edit the project</a>.</div>`;
  }

  return html`
    <form class="card" onSubmit=${submit}>
      <h2>Start run</h2>
      <div class="startform">
        <div class="field">
          <label for="sr-persona">Persona</label>
          <select id="sr-persona" value=${p} disabled=${!!active || busy}
              onChange=${(e) => { setPersona(e.currentTarget.value); setStory(''); setErr(''); }}>
            ${roles.map((r) => html`<optgroup label=${r.name}>
              ${r.personas.map((x) => html`<option value=${x.name}>${x.name}</option>`)}
            </optgroup>`)}
          </select>
        </div>
        <div class="field">
          <label for="sr-story">Story</label>
          <select id="sr-story" value=${s} disabled=${!!active || busy || !stories.length}
              onChange=${(e) => { setStory(e.currentTarget.value); setErr(''); }}>
            ${stories.length ? stories.map((x) => html`<option value=${x.name}>${x.name}</option>`)
              : html`<option value="">No stories for this role</option>`}
          </select>
        </div>
        <button class="btn primary" type="submit" disabled=${disabled}>${busy ? 'Starting…' : 'Start run'}</button>
      </div>
      ${goal ? html`<p class="goal">${goal}</p>` : null}
      ${active ? html`<div class="note">A run is active (<a href=${runHref(active.id)}>${active.persona} · ${active.story}</a>). You can start another when it finishes.</div>` : null}
      ${err ? html`<div class="note bad" role="alert">${err}</div>` : null}
    </form>`;
}

function StateTag({ state }) {
  return html`<span class=${'tag ' + (STATE_TAG.get(state) || 'idle')}>${state || 'unknown'}</span>`;
}

function RunsTable({ runs }) {
  const open = (id) => () => { location.hash = runHref(id); };
  return html`
    <div class="card flush tablewrap">
      <table>
        <thead><tr><th>Persona</th><th>Story</th><th>State</th><th>Started</th><th>Outcome</th></tr></thead>
        <tbody>
          ${runs.map((r) => html`
            <tr class="click" key=${r.id} onClick=${open(r.id)}>
              <td><a href=${runHref(r.id)} onClick=${(e) => e.stopPropagation()}>${r.persona}</a>
                ${r.role ? html`<div class="small muted">${r.role}</div>` : null}</td>
              <td>${r.story}</td>
              <td><${StateTag} state=${r.state} /></td>
              <td class="when" title=${fmtFull(r.started)}>${fmtWhen(r.started)}</td>
              <td class="outcome" title=${firstLine(r.outcome)}>${firstLine(r.outcome) || html`<span class="muted">${ACTIVE.has(r.state) ? 'In progress' : '—'}</span>`}</td>
            </tr>`)}
        </tbody>
      </table>
    </div>`;
}

// ---- run detail ---------------------------------------------------------

function RunPage({ id, onChange }) {
  const [data, setData] = useState(null);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [duration, setDuration] = useState(0); // seconds, once the video's metadata is in
  const video = useRef(null);
  const lastState = useRef('');
  const failures = useRef(0);
  const [gaveUp, setGaveUp] = useState(false);

  usePoll(async (stale) => {
    try {
      const d = await api(`/api/runs/${encodeURIComponent(id)}`);
      if (stale()) return null;
      failures.current = 0;
      setData(d); setErr(''); setMissing(false); setGaveUp(false);
      const st = d && d.run ? d.run.state : '';
      const prev = lastState.current;
      lastState.current = st;
      if (prev && prev !== st) onChange();
      if (ACTIVE.has(st)) return ACTIVE_POLL_MS;
      // Just finished: look once more for files uploaded at the end.
      return ACTIVE.has(prev) ? ACTIVE_POLL_MS : null;
    } catch (e) {
      if (stale()) return null;
      if (e.status === 404) { setMissing(true); return null; }
      setErr(e.message);
      const n = ++failures.current;
      // A finished run has nothing new to show; give up after a few tries.
      if (lastState.current && !ACTIVE.has(lastState.current) && n >= MAX_FINISHED_FAILURES) { setGaveUp(true); return null; }
      return RETRY_MS[Math.min(n, RETRY_MS.length) - 1];
    }
  }, [id]);

  if (missing) {
    return html`<section class="page"><p class="crumb"><a href="#/runs">Runs</a></p>
      <div class="empty"><b>Run not found</b>It may have been removed. <a href="#/runs">Back to runs</a>.</div></section>`;
  }
  if (!data) {
    return html`<section class="page"><p class="crumb"><a href="#/runs">Runs</a></p>
      ${err ? html`<div class="note bad">Could not load the run: ${err}</div>` : html`<p class="muted">Loading run…</p>`}</section>`;
  }

  const { run, bugs = [], artifacts = [] } = data;
  const webm = artifacts.find((n) => n.toLowerCase().endsWith('.webm'));
  const started = parseTime(run.started), finished = parseTime(run.finished);
  const took = started ? fmtDuration((finished || new Date()) - started) : '';

  const seek = (secs) => {
    const v = video.current;
    if (!v) return;
    v.currentTime = secs;
    v.play().catch(() => {});
    v.focus();
  };

  return html`
    <section class="page">
      <p class="crumb"><a href="#/runs">Runs</a> / ${run.id}</p>
      <h1>${run.persona} · ${run.story} <${StateTag} state=${run.state} /></h1>
      <p class="lede">${run.role ? `${run.role}. ` : ''}Started ${fmtFull(run.started)}${finished ? `, took ${took}` : took ? `, ${took} so far` : ''}.</p>
      ${err ? html`<div class="note bad banner">Lost contact with the director: ${err}. ${gaveUp ? 'Reload the page to try again.' : 'Retrying.'}</div>` : null}
      <div class="grid g-side">
        <div class="stack">
          <div class="player">
            ${webm ? html`<video ref=${video} key=${webm} src=${artifactURL(run.id, webm)} controls preload="metadata" playsinline
                onLoadedMetadata=${(e) => { const d = e.currentTarget.duration; setDuration(d > 0 ? d : 0); }}></video>`
              : html`<p>${ACTIVE.has(run.state) ? 'The recording appears here when the run finishes.' : 'This run has no recording.'}</p>`}
          </div>
          <div class="card">
            <div class="row between"><h2>Bugs</h2><span class="small muted">${bugs.length && webm ? 'Click a bug with a time to jump to it.' : ''}</span></div>
            ${bugs.length ? html`<ul class="bugs">${bugs.map((b) => html`<li key=${b.id}><${Bug} bug=${b} duration=${webm ? duration : 0} onSeek=${seek} /></li>`)}</ul>`
              : html`<p class="muted small" style="margin:0">${ACTIVE.has(run.state) ? 'No bugs reported so far.' : 'No bugs reported.'}</p>`}
          </div>
        </div>
        <div class="stack">
          <div class="card">
            <h2>Outcome</h2>
            ${run.outcome ? html`<p class="outcome-text">${run.outcome}</p>`
              : html`<p class="muted small" style="margin:0">${ACTIVE.has(run.state) ? 'Still running.' : 'No outcome recorded.'}</p>`}
          </div>
          <div class="card">
            <h2>Details</h2>
            <dl class="facts">
              <dt>Persona</dt><dd>${run.persona}</dd>
              ${run.role ? html`<dt>Role</dt><dd>${run.role}</dd>` : null}
              <dt>Story</dt><dd>${run.story}</dd>
              <dt>State</dt><dd><${StateTag} state=${run.state} /></dd>
              <dt>Started</dt><dd>${fmtFull(run.started) || '—'}</dd>
              <dt>Finished</dt><dd>${fmtFull(run.finished) || '—'}</dd>
              ${took ? html`<dt>Duration</dt><dd>${took}</dd>` : null}
              ${run.session ? html`<dt>Session</dt><dd><code>${run.session}</code></dd>` : null}
            </dl>
          </div>
          <div class="card">
            <h2>Files</h2>
            ${artifacts.length ? html`<ul class="files">${artifacts.map((n) => html`
              <li key=${n}><a href=${artifactURL(run.id, n)} download=${n}>${n}</a>
                ${n === 'trace.zip' ? html`<span class="hint">Open it with <code>npx playwright show-trace trace.zip</code></span>` : null}</li>`)}</ul>`
              : html`<p class="muted small" style="margin:0">No files yet.</p>`}
          </div>
        </div>
      </div>
    </section>`;
}

const SEVERITY_TAG = new Map([['high', 'bad'], ['critical', 'bad'], ['medium', 'wait'], ['low', 'idle']]);

// A bug seeks only when its text carries a time that fits inside the
// recording; that also keeps clock times such as "Wed 16:00" from seeking.
// A live-recorded webm can report Infinity, which lets every time through.
function Bug({ bug, duration, onSeek }) {
  const found = stampOf(bug);
  const secs = found !== null && duration > 0 && found <= Math.ceil(duration) ? found : null;
  const seekable = secs !== null;
  const body = html`
    <span class=${'tag ' + (SEVERITY_TAG.get((bug.severity || '').toLowerCase()) || 'idle')}>${bug.severity || 'unrated'}</span>
    <span class="sum">${bug.summary}</span>
    ${secs !== null ? html`<span class="stamp">${fmtStamp(secs)}</span>` : html`<span></span>`}
    ${bug.step ? html`<span class="meta"><b>Step:</b> ${bug.step}</span>` : null}
    ${bug.evidence ? html`<span class="meta"><b>Evidence:</b> ${bug.evidence}</span>` : null}
    ${bug.note ? html`<span class="meta"><b>Note:</b> ${bug.note}</span>` : null}`;
  return seekable
    ? html`<button type="button" class="bug seek" title=${'Jump to ' + fmtStamp(secs)} onClick=${() => onSeek(secs)}>${body}</button>`
    : html`<div class="bug">${body}</div>`;
}

// ---- project (read-only until the editor lands) -------------------------

function ProjectPage({ project, error }) {
  if (error) return html`<section class="page"><h1>Project</h1><div class="note bad">Could not load the project: ${error}</div></section>`;
  if (project === undefined) return html`<section class="page"><h1>Project</h1><p class="muted">Loading…</p></section>`;
  if (project === null) {
    return html`<section class="page"><h1>Project</h1>
      <div class="empty"><b>No project yet</b>Upload one with <code>boardingparty apply -f project.yaml</code>.</div></section>`;
  }
  return html`
    <section class="page">
      <h1>${project.name || 'Project'}</h1>
      <p class="lede">What the cast may open and who is in it.</p>
      <div class="stack">
        <div class="card">
          <h2>Sites</h2>
          <ul class="files">${(project.sites || []).map((s) => html`<li key=${s}><code>${s}</code></li>`)}</ul>
          ${(project.extraHosts || []).length ? html`<p class="small muted" style="margin:8px 0 0">Also allowed: ${project.extraHosts.join(', ')}</p>` : null}
          ${project.model ? html`<p class="small muted" style="margin:8px 0 0">Model: ${project.model}</p>` : null}
        </div>
        <div class="card flush tablewrap">
          <table>
            <thead><tr><th>Role</th><th>Personas</th><th>Stories</th></tr></thead>
            <tbody>${(project.roles || []).map((r) => html`
              <tr key=${r.name}><td><b>${r.name}</b>${r.description ? html`<div class="small muted">${r.description}</div>` : null}</td>
                <td>${(r.personas || []).map((p) => p.name).join(', ') || html`<span class="muted">none</span>`}</td>
                <td>${(r.stories || []).map((s) => s.name).join(', ') || html`<span class="muted">none</span>`}</td></tr>`)}
            </tbody>
          </table>
        </div>
      </div>
    </section>`;
}

render(html`<${App} />`, document.getElementById('root'));
