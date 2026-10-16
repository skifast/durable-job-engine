// Pure helpers for the operator page: what the tabs are, what text to show for an error, how to
// check what the operator typed, how to read the address bar. Nothing here touches the page.

export const TABS = [
  { id: 'waiting_approval', label: 'Waiting approval', query: { state: 'waiting_approval' } },
  { id: 'escalated', label: 'Escalated', query: { escalated: true } },
  { id: 'dead', label: 'Dead', query: { state: 'dead' } },
  { id: 'running', label: 'Running', query: { state: 'running' } },
  { id: 'queued', label: 'Queued', query: { state: 'queued' } },
  { id: 'succeeded', label: 'Succeeded', query: { state: 'succeeded' } },
  { id: 'rejected', label: 'Rejected', query: { state: 'rejected' } },
];

// Only waiting_approval and dead lists are indexed (ADR 011, docs/api.md); the rest can be slow
// on a large table.
export const SLOW_TABS = new Set(['running', 'queued', 'succeeded', 'rejected']);

export const DEFAULT_TAB = 'waiting_approval';
export const MAX_REASON_CHARS = 1000;
export const MAX_REDRIVE_ATTEMPTS = 100;

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
export const isJobId = (s) => typeof s === 'string' && UUID.test(s);

export function tabById(id) { return TABS.find((t) => t.id === id) || null; }

// ---- The address bar (a hash, so a reload and the back button work) ----

export const jobsHash = (tab) => '#/jobs/' + tab;
export const jobHash = (id) => '#/job/' + id;

// parseRoute reads location.hash. Anything it does not understand becomes the default list.
export function parseRoute(hash) {
  const parts = String(hash || '').replace(/^#\/?/, '').split('/');
  if (parts[0] === 'job' && isJobId(parts[1])) return { name: 'job', id: parts[1] };
  if (parts[0] === 'jobs' && tabById(parts[1])) return { name: 'jobs', tab: parts[1] };
  return { name: 'jobs', tab: DEFAULT_TAB };
}

// ---- What an operator can do with a job ----

// jobActions: the actions that can work on a job in this state (docs/api.md).
export function jobActions(job) {
  if (job.state === 'waiting_approval') return ['approve', 'reject'];
  if (job.state === 'dead') return ['redrive'];
  return [];
}

// ---- Checking what was typed ----

// validateReason: required for a reject, optional for an approve. Returns { ok, value, error }.
export function validateReason(text, { required }) {
  const value = String(text || '').trim();
  if (required && value === '') return { ok: false, error: 'A reject needs a reason.' };
  if ([...value].length > MAX_REASON_CHARS) {
    return { ok: false, error: 'The reason is over ' + MAX_REASON_CHARS + ' characters.' };
  }
  return { ok: true, value };
}

// validateRedrive: a whole number greater than the attempts already made, at most 100.
export function validateRedrive(text, attempt) {
  const s = String(text || '').trim();
  if (!/^\d+$/.test(s)) return { ok: false, error: 'Enter a whole number.' };
  const n = Number(s);
  if (n <= attempt) {
    return { ok: false, error: 'Must be greater than ' + attempt + ', the attempts already made.' };
  }
  if (n > MAX_REDRIVE_ATTEMPTS) {
    return { ok: false, error: 'At most ' + MAX_REDRIVE_ATTEMPTS + '.' };
  }
  return { ok: true, value: n };
}

export function redriveHint(attempt) {
  return 'More than ' + attempt + ', at most ' + MAX_REDRIVE_ATTEMPTS + '.';
}

// ---- What to show when a call fails ----

// describeError: the engine's own message for a refusal, plus the details it sent (for example
// the job's current state), so a stale click explains itself. Failures that have no message of
// the engine's (no answer, a rejected key) get one written here.
export function describeError(err) {
  if (err.status === 0) return 'Could not reach the engine. Check that it is running, then try again.';
  if (err.status === 401) return 'The engine did not accept this key. It may have been revoked.';
  const bits = Object.entries(err.details || {}).map(([k, v]) => k + ': ' + v);
  return err.message + (bits.length ? ' (' + bits.join(', ') + ')' : '');
}

// ---- Showing values ----

// formatTime: "2026-10-16 03:25:53 UTC" from the API's RFC 3339 text, or a dash for nothing.
export function formatTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return String(iso);
  return d.toISOString().slice(0, 19).replace('T', ' ') + ' UTC';
}

export function shortId(id) { return String(id).slice(0, 8); }

export function truncate(text, max) {
  const chars = [...String(text)];
  return chars.length <= max ? chars.join('') : chars.slice(0, max - 1).join('') + '…';
}

// prettyJson: a JSON value as indented text, or a dash when there is none.
export function prettyJson(value) {
  return value === null || value === undefined ? '—' : JSON.stringify(value, null, 2);
}

export function isEscalated(job) {
  return job.state === 'waiting_approval' && Boolean(job.escalated_at);
}

// lastErrorText: one line for a list row.
export function lastErrorText(summary) {
  const e = summary.last_error;
  return e ? e.kind + ': ' + truncate(e.message, 80) : '—';
}
