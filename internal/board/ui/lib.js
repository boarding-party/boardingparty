// Shared bits for the board UI: the htm binding and the API client.
//
// Server text is untrusted: render it only as text through htm/Preact.
import { h } from './vendor/preact.module.js';
import htm from './vendor/htm.module.js';
import { useState, useEffect, useRef } from './vendor/hooks.module.js';

export const html = htm.bind(h);

// ---- API ----------------------------------------------------------------

export class APIError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

// api calls the director through the board's proxy. Non-2xx responses throw
// an APIError carrying the server's message verbatim (JSON {"error"} or the
// plain-text body http.Error writes).
export async function api(path, opts = {}) {
  let res;
  try {
    res = await fetch(path, { credentials: 'same-origin', ...opts });
  } catch (e) {
    throw new APIError(0, 'Cannot reach the board server. Is boardingparty board still running?');
  }
  if (res.status === 204) return null;
  const text = await res.text();
  if (!res.ok) {
    let msg = text.trim();
    try { const j = JSON.parse(text); if (j && typeof j.error === 'string') msg = j.error; } catch (_) { /* plain text */ }
    throw new APIError(res.status, msg || res.statusText || `HTTP ${res.status}`);
  }
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch (_) {
    throw new APIError(res.status, `The director sent a response that isn't JSON (HTTP ${res.status}).`);
  }
}

export const directorDown = (e) => e instanceof APIError && (e.status === 0 || e.status === 502 || e.status === 503 || e.status === 504);

// ---- confirm dialog -------------------------------------------------------
// confirmDialog(message, {confirm, cancel}) resolves true or false. It renders
// in <DialogHost/> (mounted once by App) as a native modal <dialog>: no
// window.confirm, no inline handlers, so it works under the board's CSP.

let openDialog = null;

// dialogOpen reports whether a confirm dialog is showing.
export const dialogOpen = () => !!document.querySelector('dialog.confirm[open]');

export function confirmDialog(message, opts = {}) {
  return new Promise((resolve) => {
    if (!openDialog) { resolve(false); return; }
    openDialog({ message, confirm: opts.confirm || 'Continue', cancel: opts.cancel || 'Cancel', danger: !!opts.danger, resolve });
  });
}

export function DialogHost() {
  const [d, setD] = useState(null);
  const ref = useRef(null);
  useEffect(() => {
    openDialog = (next) => setD((cur) => { if (cur) cur.resolve(false); return next; });
    return () => { openDialog = null; };
  }, []);
  useEffect(() => {
    const el = ref.current;
    if (d && el && !el.open) {
      el.showModal();
      // Focus the safe choice: a stray Enter must not discard or remove.
      const safe = el.querySelector('[data-cancel]');
      if (safe) safe.focus();
    }
  }, [d]);
  if (!d) return null;
  const close = (v) => {
    const el = ref.current;
    if (el && el.open) el.close();
    setD(null);
    d.resolve(v);
  };
  return html`
    <dialog ref=${ref} class="confirm" aria-labelledby="confirm-msg"
        onCancel=${(e) => { e.preventDefault(); close(false); }}>
      <p id="confirm-msg">${d.message}</p>
      <div class="row end">
        <button class="btn" type="button" data-cancel onClick=${() => close(false)}>${d.cancel}</button>
        <button class=${'btn ' + (d.danger ? 'danger-solid' : 'primary')} type="button" data-ok onClick=${() => close(true)}>${d.confirm}</button>
      </div>
    </dialog>`;
}
