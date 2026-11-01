// Project editor: sites, roles, personas, stories. Saves the whole project
// with PUT /api/project (JSON); "Import YAML" posts a pasted file as-is.
//
// Field names must match internal/project/project.go exactly: the director
// rejects unknown fields.
import { useState, useEffect, useRef } from './vendor/hooks.module.js';
import { html, api } from './lib.js';

// ---- model --------------------------------------------------------------
// The draft carries a stable id on every repeatable row so inputs keep focus
// while rows are added and removed. toJSON strips them.

let seq = 0;
const nid = () => `r${++seq}`;

const row = (v = '') => ({ id: nid(), v });
const newPersona = () => ({ id: nid(), name: '', traits: [], identity: [] });
const newStory = () => ({ id: nid(), name: '', goal: '' });
const newRole = () => ({ id: nid(), name: '', description: '', canWatchRecordings: false, personas: [newPersona()], stories: [newStory()] });

function fromProject(p) {
  p = p || {};
  const sites = (p.sites || []).map(row);
  return {
    name: p.name || '',
    sites: sites.length ? sites : [row()],
    extraHosts: (p.extraHosts || []).map(row),
    model: p.model || '',
    roles: (p.roles || []).map((r) => ({
      id: nid(),
      name: r.name || '',
      description: r.description || '',
      canWatchRecordings: !!r.canWatchRecordings,
      personas: (r.personas || []).map((pe) => ({
        id: nid(),
        name: pe.name || '',
        traits: [...(pe.traits || [])],
        identity: Object.entries(pe.identity || {}).map(([k, v]) => ({ id: nid(), k, v: String(v) })),
      })),
      stories: (r.stories || []).map((s) => ({ id: nid(), name: s.name || '', goal: s.goal || '' })),
    })),
  };
}

const vals = (rows) => rows.map((x) => x.v.trim()).filter(Boolean);

function toJSON(d) {
  return {
    name: d.name.trim(),
    sites: vals(d.sites),
    extraHosts: vals(d.extraHosts),
    model: d.model.trim(),
    roles: d.roles.map((r) => ({
      name: r.name.trim(),
      description: r.description.trim(),
      canWatchRecordings: r.canWatchRecordings,
      personas: r.personas.map((pe) => ({
        name: pe.name.trim(),
        traits: pe.traits,
        identity: Object.fromEntries(pe.identity.filter((x) => x.k.trim()).map((x) => [x.k.trim(), x.v])),
      })),
      stories: r.stories.map((s) => ({ name: s.name.trim(), goal: s.goal.trim() })),
    })),
  };
}

// siteWarning flags sites that may not be staging. It never blocks a save;
// the director still enforces https://.
function siteWarning(s) {
  s = s.trim();
  if (!s) return '';
  let u;
  try { u = new URL(s); } catch (_) { return 'This is not a full URL. Make sure this is a staging site.'; }
  if (u.protocol !== 'https:') return 'This is not https://. Make sure this is a staging site.';
  const host = u.hostname.toLowerCase();
  if (host.startsWith('www.') || !/(staging|test|dev)/.test(host)) {
    return 'This looks like a production address. Make sure this is a staging site.';
  }
  return '';
}

// ---- page ---------------------------------------------------------------

export function ProjectPage({ onSaved }) {
  const [state, setState] = useState('loading'); // loading | ready | error
  const [loadErr, setLoadErr] = useState('');
  const [isNew, setIsNew] = useState(false);
  const [draft, setDraft] = useState(null);
  const [baseline, setBaseline] = useState('');
  const [saving, setSaving] = useState(false);
  const [saveErr, setSaveErr] = useState('');
  const [toast, setToast] = useState('');
  const [importOpen, setImportOpen] = useState(false);
  const top = useRef(null);

  const adopt = (p) => {
    const d = fromProject(p);
    setDraft(d);
    setBaseline(JSON.stringify(toJSON(d)));
    setIsNew(!p);
  };

  const load = async () => {
    try {
      adopt(await api('/api/project'));
      setState('ready');
    } catch (e) {
      if (e.status === 404) { adopt(null); setState('ready'); return; }
      setLoadErr(e.message); setState('error');
    }
  };

  useEffect(() => { load(); }, []);

  const dirty = draft && JSON.stringify(toJSON(draft)) !== baseline;

  useEffect(() => {
    if (!dirty) return undefined;
    const warn = (e) => { e.preventDefault(); e.returnValue = ''; };
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, [dirty]);

  useEffect(() => {
    if (!toast) return undefined;
    const t = setTimeout(() => setToast(''), 3000);
    return () => clearTimeout(t);
  }, [toast]);

  // edit applies fn to a copy of the draft.
  const edit = (fn) => setDraft((d) => { const c = structuredClone(d); fn(c); return c; });

  const save = async (e) => {
    e.preventDefault();
    setSaving(true); setSaveErr('');
    try {
      await api('/api/project', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(toJSON(draft)),
      });
      setBaseline(JSON.stringify(toJSON(draft)));
      setIsNew(false);
      setToast('Project saved');
      onSaved();
    } catch (e2) {
      setSaveErr(e2.message);
      if (top.current) top.current.scrollIntoView({ behavior: 'smooth', block: 'start' });
    } finally {
      setSaving(false);
    }
  };

  const imported = async () => {
    setImportOpen(false);
    setSaveErr('');
    await load();
    setToast('Project imported');
    onSaved();
  };

  if (state === 'loading') return html`<section class="page"><h1>Project</h1><p class="muted">Loading…</p></section>`;
  if (state === 'error') {
    return html`<section class="page"><h1>Project</h1>
      <div class="note bad">Could not load the project: ${loadErr}</div>
      <div style="margin-top:12px"><button class="btn" type="button" onClick=${() => { setState('loading'); load(); }}>Try again</button></div></section>`;
  }

  return html`
    <form class="page editor" onSubmit=${save} ref=${top}>
      <div class="row between head">
        <div>
          <h1>${isNew ? 'Set up your project' : 'Project'}</h1>
          <p class="lede">The site the cast may open, and who is in it. Saving replaces the whole project.</p>
        </div>
        <div class="row">
          ${dirty ? html`<span class="small muted">Unsaved changes</span>` : null}
          <button class="btn" type="button" aria-expanded=${importOpen} onClick=${() => setImportOpen(!importOpen)}>Import YAML</button>
          <button class="btn primary" type="submit" disabled=${saving}>${saving ? 'Saving…' : 'Save'}</button>
        </div>
      </div>

      ${saveErr ? html`<div class="note bad banner" role="alert"><b>Not saved.</b> ${saveErr}</div>` : null}
      ${importOpen ? html`<${ImportYAML} dirty=${dirty} onDone=${imported} onCancel=${() => setImportOpen(false)} />` : null}

      <div class="stack">
        <div class="card">
          <h2>Site</h2>
          <div class="fields">
            <div class="field">
              <label for="pj-name">Project name</label>
              <input id="pj-name" value=${draft.name} placeholder="shop-staging"
                onInput=${(e) => { const v = e.currentTarget.value; edit((d) => { d.name = v; }); }} />
            </div>
            <div class="field">
              <label for="pj-model">Model</label>
              <input id="pj-model" value=${draft.model} placeholder="Default"
                onInput=${(e) => { const v = e.currentTarget.value; edit((d) => { d.model = v; }); }} />
              <span class="hint">Leave empty to use the default model.</span>
            </div>
          </div>
          <${ListField} label="Sites" hint="Staging URLs the cast may open, starting with https://. Never point it at production."
            rows=${draft.sites} placeholder="https://staging.example.com" addLabel="Add site" warn=${siteWarning}
            onChange=${(fn) => edit((d) => fn(d.sites))} />
          <${ListField} label="Extra hosts" hint="Other hosts the site loads from, such as a CDN or a sign-in provider."
            rows=${draft.extraHosts} placeholder="cdn.example.com" addLabel="Add host"
            onChange=${(fn) => edit((d) => fn(d.extraHosts))} />
        </div>

        ${draft.roles.map((r, i) => html`<${RoleCard} key=${r.id} role=${r} n=${i + 1}
            onChange=${(fn) => edit((d) => fn(d.roles[i]))}
            onRemove=${() => {
              const busy = r.personas.some((p) => p.name.trim()) || r.stories.some((s) => s.name.trim() || s.goal.trim());
              if (busy && !window.confirm(`Remove the role ${r.name.trim() || 'without a name'} with its personas and stories?`)) return;
              edit((d) => { d.roles.splice(i, 1); });
            }} />`)}

        ${draft.roles.length ? null : html`<div class="empty"><b>No roles yet</b>A role is a kind of user, such as a customer or an admin. Each has personas and stories.</div>`}

        <div class="row between">
          <button class="btn" type="button" onClick=${() => edit((d) => { d.roles.push(newRole()); })}>Add role</button>
          <button class="btn primary" type="submit" disabled=${saving}>${saving ? 'Saving…' : 'Save'}</button>
        </div>
      </div>
      ${toast ? html`<div class="toast" role="status">${toast}</div>` : null}
    </form>`;
}

// ---- pieces -------------------------------------------------------------

function ListField({ label, hint, rows, placeholder, addLabel, warn, onChange }) {
  return html`
    <div class="field">
      <label>${label}</label>
      ${hint ? html`<span class="hint">${hint}</span>` : null}
      <div class="list">
        ${rows.map((x, i) => {
          const w = warn ? warn(x.v) : '';
          return html`<div key=${x.id}>
            <div class="row nowrap">
              <input value=${x.v} placeholder=${placeholder} aria-label=${label + ' ' + (i + 1)}
                onInput=${(e) => { const v = e.currentTarget.value; onChange((l) => { l[i].v = v; }); }} />
              <button class="btn icon" type="button" title="Remove" aria-label=${'Remove ' + (x.v || 'row')}
                onClick=${() => onChange((l) => { l.splice(i, 1); })}>×</button>
            </div>
            ${w ? html`<div class="warn">${w}</div>` : null}
          </div>`;
        })}
      </div>
      <div><button class="btn ghost" type="button" onClick=${() => onChange((l) => { l.push(row()); })}>${addLabel}</button></div>
    </div>`;
}

function RoleCard({ role, n, onChange, onRemove }) {
  const id = `role-${role.id}`;
  return html`
    <div class="card role">
      <div class="row between nowrap">
        <div class="field grow">
          <label for=${id + '-name'}>Role ${n}</label>
          <input id=${id + '-name'} class="strong" value=${role.name} placeholder="Customer"
            onInput=${(e) => { const v = e.currentTarget.value; onChange((r) => { r.name = v; }); }} />
        </div>
        <button class="btn danger-ghost" type="button" onClick=${onRemove}>Remove role</button>
      </div>
      <div class="field">
        <label for=${id + '-desc'}>Description</label>
        <textarea id=${id + '-desc'} rows="2" value=${role.description} placeholder="Who they are and what they want from the site."
          onInput=${(e) => { const v = e.currentTarget.value; onChange((r) => { r.description = v; }); }}></textarea>
      </div>
      <label class="check">
        <input type="checkbox" checked=${role.canWatchRecordings}
          onChange=${(e) => { const v = e.currentTarget.checked; onChange((r) => { r.canWatchRecordings = v; }); }} />
        <span>Can watch other roles' recordings <span class="hint">Turns this role into a supervisor.</span></span>
      </label>

      <div class="cols">
        <div>
          <h3>Personas</h3>
          ${role.personas.length ? null : html`<p class="small muted">No personas. Runs need at least one.</p>`}
          ${role.personas.map((p, i) => html`<${PersonaRow} key=${p.id} persona=${p}
              onChange=${(fn) => onChange((r) => fn(r.personas[i]))}
              onRemove=${() => onChange((r) => { r.personas.splice(i, 1); })} />`)}
          <button class="btn ghost" type="button" onClick=${() => onChange((r) => { r.personas.push(newPersona()); })}>Add persona</button>
        </div>
        <div>
          <h3>Stories</h3>
          ${role.stories.length ? null : html`<p class="small muted">No stories. Runs need at least one.</p>`}
          ${role.stories.map((s, i) => html`<${StoryRow} key=${s.id} story=${s}
              onChange=${(fn) => onChange((r) => fn(r.stories[i]))}
              onRemove=${() => onChange((r) => { r.stories.splice(i, 1); })} />`)}
          <button class="btn ghost" type="button" onClick=${() => onChange((r) => { r.stories.push(newStory()); })}>Add story</button>
        </div>
      </div>
    </div>`;
}

function PersonaRow({ persona, onChange, onRemove }) {
  const id = `p-${persona.id}`;
  return html`
    <div class="sub">
      <div class="row nowrap">
        <input id=${id} class="strong" value=${persona.name} placeholder="Persona name" aria-label="Persona name"
          onInput=${(e) => { const v = e.currentTarget.value; onChange((p) => { p.name = v; }); }} />
        <button class="btn icon" type="button" title="Remove persona" aria-label=${'Remove persona ' + persona.name}
          onClick=${onRemove}>×</button>
      </div>
      <${Tags} tags=${persona.traits} label="Traits" placeholder="Add a trait, such as impatient or phone"
        onChange=${(fn) => onChange((p) => fn(p.traits))} />
      <div class="kv">
        <span class="sublabel">Identity</span>
        ${persona.identity.map((x, i) => html`<div class="row nowrap" key=${x.id}>
          <input class="k" value=${x.k} placeholder="email" aria-label="Identity field"
            onInput=${(e) => { const v = e.currentTarget.value; onChange((p) => { p.identity[i].k = v; }); }} />
          <input value=${x.v} placeholder="value" aria-label=${'Value for ' + (x.k || 'field')}
            onInput=${(e) => { const v = e.currentTarget.value; onChange((p) => { p.identity[i].v = v; }); }} />
          <button class="btn icon" type="button" title="Remove field" aria-label=${'Remove ' + (x.k || 'field')}
            onClick=${() => onChange((p) => { p.identity.splice(i, 1); })}>×</button>
        </div>`)}
        <div><button class="btn ghost small" type="button" onClick=${() => onChange((p) => { p.identity.push({ id: nid(), k: '', v: '' }); })}>Add identity field</button></div>
      </div>
    </div>`;
}

function StoryRow({ story, onChange, onRemove }) {
  return html`
    <div class="sub">
      <div class="row nowrap">
        <input class="strong" value=${story.name} placeholder="Story name" aria-label="Story name"
          onInput=${(e) => { const v = e.currentTarget.value; onChange((s) => { s.name = v; }); }} />
        <button class="btn icon" type="button" title="Remove story" aria-label=${'Remove story ' + story.name}
          onClick=${onRemove}>×</button>
      </div>
      <textarea rows="3" value=${story.goal} placeholder="What the persona is trying to get done, in their words." aria-label="Goal"
        onInput=${(e) => { const v = e.currentTarget.value; onChange((s) => { s.goal = v; }); }}></textarea>
    </div>`;
}

// Tags edits a list of short strings: Enter or comma adds, Backspace on an
// empty input removes the last one.
function Tags({ tags, label, placeholder, onChange }) {
  const [text, setText] = useState('');
  // add reads the input itself: a key can arrive before the render that
  // would have updated text.
  const add = (e) => {
    const t = e.currentTarget.value.trim().replace(/,$/, '').trim();
    if (t && !tags.includes(t)) onChange((l) => { l.push(t); });
    e.currentTarget.value = '';
    setText('');
  };
  return html`
    <div class="tags" aria-label=${label}>
      ${tags.map((t, i) => html`<span class="chip" key=${t}>${t}
        <button type="button" aria-label=${'Remove ' + t} onClick=${() => onChange((l) => { l.splice(i, 1); })}>×</button></span>`)}
      <input value=${text} placeholder=${tags.length ? 'Add trait' : placeholder} aria-label=${'Add to ' + label}
        onInput=${(e) => setText(e.currentTarget.value)}
        onKeyDown=${(e) => {
          if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); add(e); }
          else if (e.key === 'Backspace' && !e.currentTarget.value && tags.length) onChange((l) => { l.pop(); });
        }}
        onBlur=${add} />
    </div>`;
}

function ImportYAML({ dirty, onDone, onCancel }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const go = async () => {
    if (!text.trim()) { setErr('Paste a project YAML file first.'); return; }
    if (dirty && !window.confirm('Importing replaces the project and drops your unsaved changes. Continue?')) return;
    setBusy(true); setErr('');
    try {
      await api('/api/project', { method: 'POST', headers: { 'Content-Type': 'application/yaml' }, body: text });
      await onDone();
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };
  return html`
    <div class="card import">
      <h2>Import YAML</h2>
      <p class="small muted" style="margin:0 0 8px">Paste a project file, the same format as <code>boardingparty apply -f project.yaml</code>. It replaces the whole project.</p>
      <textarea rows="10" class="mono" value=${text} spellcheck="false" aria-label="Project YAML"
        placeholder=${'name: shop-staging\nsites:\n  - https://staging.example.com\nroles:\n  - name: Customer\n    personas:\n      - name: Sam Jones\n    stories:\n      - name: Buy a gift\n        goal: Buy a gift under £30 and have it sent to a friend.'}
        onInput=${(e) => setText(e.currentTarget.value)}></textarea>
      ${err ? html`<div class="note bad" role="alert"><b>Not imported.</b> ${err}</div>` : null}
      <div class="row" style="margin-top:10px">
        <button class="btn primary" type="button" disabled=${busy} onClick=${go}>${busy ? 'Importing…' : 'Import'}</button>
        <button class="btn" type="button" onClick=${onCancel}>Cancel</button>
      </div>
    </div>`;
}
