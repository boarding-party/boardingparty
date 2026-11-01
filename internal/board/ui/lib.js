// Shared bits for the board UI: the htm binding and the API client.
//
// Server text is untrusted: render it only as text through htm/Preact.
import { h } from './vendor/preact.module.js';
import htm from './vendor/htm.module.js';

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
