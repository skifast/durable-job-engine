// The operator page (ADR 010, ADR 015): sign in, lists of jobs by state, one job with its
// actions. This file builds the screens; what the page says and checks is in format.mjs, and
// the calls to the engine are in client.mjs.
//
// Rule: text that came from a job (payload, result, error messages, names) is only ever put
// on the page as text. There is no innerHTML here, and a test fails if one appears.

import { ApiError, createClient } from './client.mjs';
import {
  DEFAULT_TAB, MAX_REASON_CHARS, SLOW_TABS, TABS, describeError, formatTime, isEscalated, isJobId,
  jobActions, jobHash, jobsHash, lastErrorText, parseRoute, prettyJson, redriveHint, shortId, tabById,
  validateReason, validateRedrive,
} from './format.mjs';

const client = createClient();
let me = null; // { name, role } once signed in

const viewEl = document.getElementById('view');
const whoEl = document.getElementById('who');

// el builds an element. Strings among the children become text nodes, never markup.
function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v === false || v === null || v === undefined) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c !== null && c !== undefined && c !== false) node.append(c);
  }
  return node;
}

// show replaces the screen. Empty slots (null, undefined) are skipped: replaceChildren would write them as the text "null".
function show(...nodes) { viewEl.replaceChildren(...nodes.filter((n) => n !== null && n !== undefined && n !== false)); }
function banner(kind, text) { return el('div', { class: 'banner ' + kind, role: kind === 'error' ? 'alert' : 'status', text }); }
function badge(state) { return el('span', { class: 'badge ' + state, text: state.replace('_', ' ') }); }

// ---------------------------------------------------------------------------
// Signing in and out
// ---------------------------------------------------------------------------

function drawWho() {
  if (!me) { whoEl.hidden = true; whoEl.replaceChildren(); return; }
  whoEl.hidden = false;
  whoEl.replaceChildren(
    el('span', {}, 'acting as ', el('strong', { text: me.name }), ' (' + me.role + ')'),
    el('button', { type: 'button', text: 'Sign out', onclick: () => signOut() }),
  );
}

function signOut(message) {
  client.setKey(null);
  me = null;
  drawWho();
  renderSignIn(message ? banner('error', message) : null);
}

function renderSignIn(notice) {
  const input = el('input', { id: 'key', type: 'password', autocomplete: 'off', required: true, spellcheck: 'false' });
  const status = el('div', { 'aria-live': 'polite' });
  const button = el('button', { class: 'primary', type: 'submit', text: 'Sign in' });
  const form = el('form', { class: 'card signin' },
    el('h2', { text: 'Sign in with an operator key' }),
    notice,
    el('p', { class: 'muted', text: 'The key is kept in this page\'s memory only. A reload asks for it again. ' +
      'The engine speaks plain HTTP: anywhere beyond local use, put a TLS proxy in front, or the key crosses the network in the clear.' }),
    el('div', { class: 'field' }, el('label', { for: 'key', text: 'API key' }), input),
    button, status);

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const key = input.value.trim();
    input.value = ''; // do not leave the key sitting in the page
    if (!key) return;
    button.disabled = true;
    status.replaceChildren(el('p', { text: 'Checking the key…' }));
    client.setKey(key);
    try {
      const who = await client.whoami();
      if (who.role !== 'operator') {
        client.setKey(null);
        status.replaceChildren(banner('error',
          'That is a ' + who.role + ' key (' + who.name + '). The page needs an operator key.'));
        button.disabled = false;
        return;
      }
      me = who;
      drawWho();
      route();
    } catch (e) {
      client.setKey(null);
      status.replaceChildren(banner('error', e instanceof ApiError ? describeError(e) : 'Something went wrong.'));
      button.disabled = false;
    }
  });
  show(form);
  input.focus();
}

// Any call can find the key revoked. Go back to sign-in, and say so.
function handleCallError(e, onOther) {
  if (e instanceof ApiError && e.status === 401) { signOut(describeError(e)); return; }
  onOther(e instanceof ApiError ? describeError(e) : 'Something went wrong in the page: ' + e.message);
}

// ---------------------------------------------------------------------------
// The list screen
// ---------------------------------------------------------------------------

let listToken = 0; // a newer request makes an older answer irrelevant

function renderList(tabId) {
  const tab = tabById(tabId);
  const token = ++listToken;
  let jobs = [];
  let next = null;
  let loading = false;

  const tabs = el('nav', { class: 'tabs', 'aria-label': 'Job lists' },
    TABS.map((t) => el('a', { href: jobsHash(t.id), text: t.label, 'aria-current': t.id === tab.id ? 'page' : false })));
  const message = el('div');
  const tbody = el('tbody');
  const more = el('button', { type: 'button', text: 'Load more', hidden: true });
  const refresh = el('button', { type: 'button', text: 'Refresh' });
  const count = el('span', { class: 'muted' });
  const showDeadline = tab.id === 'waiting_approval' || tab.id === 'escalated';

  const head = el('tr', {}, ['Job', 'Type', 'Client', 'State', 'Attempts', showDeadline ? 'Deadline' : 'Created', 'Last error']
    .map((h) => el('th', { scope: 'col', text: h })));
  const empty = el('div', { class: 'empty', text: 'No jobs in this list.', hidden: true });

  function draw() {
    tbody.replaceChildren(...jobs.map((j) => el('tr', {},
      el('td', { class: 'mono' }, el('a', { href: isJobId(j.id) ? jobHash(j.id) : false, text: shortId(j.id) })),
      el('td', { text: j.type }),
      el('td', { text: j.client_id }),
      el('td', {}, badge(j.state), isEscalated(j) ? el('span', { class: 'badge escalated', text: 'escalated' }) : null),
      el('td', { text: j.attempt + ' / ' + j.max_attempts }),
      el('td', { text: formatTime(showDeadline ? j.approval_deadline : j.created_at) }),
      el('td', { text: lastErrorText(j) }))));
    count.textContent = jobs.length + ' loaded';
    empty.hidden = loading || jobs.length > 0;
    more.hidden = next === null;
    more.disabled = loading;
    refresh.disabled = loading;
  }

  async function load(reset) {
    loading = true;
    if (reset) { jobs = []; next = null; }
    message.replaceChildren(loading && reset ? el('p', { class: 'muted', text: 'Loading…' }) : '');
    draw();
    try {
      const page = await client.listJobs({ ...tab.query, cursor: reset ? undefined : next, limit: 50 });
      if (token !== listToken) return; // the operator moved on
      jobs = jobs.concat(page.jobs);
      next = page.next_cursor;
      message.replaceChildren();
    } catch (e) {
      if (token !== listToken) return;
      handleCallError(e, (text) => message.replaceChildren(banner('error', text)));
      if (!me) return;
    } finally {
      if (token === listToken) { loading = false; draw(); }
    }
  }

  refresh.addEventListener('click', () => load(true));
  more.addEventListener('click', () => load(false));

  show(
    tabs,
    SLOW_TABS.has(tab.id) ? banner('info', 'This list is not indexed and can be slow on a large table.') : null,
    el('div', { class: 'toolbar' }, refresh, count, el('span', { class: 'spacer' })),
    message,
    el('div', { class: 'card table-wrap' }, el('table', {}, el('thead', {}, head), tbody), empty),
    el('div', { class: 'toolbar' }, more));
  document.title = tab.label + ' · Job engine';
  load(true);
}

// ---------------------------------------------------------------------------
// The job screen
// ---------------------------------------------------------------------------

function facts(rows) {
  return el('dl', { class: 'facts' }, rows.flatMap(([k, v]) => [el('dt', { text: k }), el('dd', {}, v)]));
}
const text = (v) => (v === null || v === undefined || v === '' ? '—' : String(v));

function renderJob(id) {
  const token = ++listToken;
  show(el('p', { class: 'muted', text: 'Loading…' }));
  document.title = 'Job ' + shortId(id) + ' · Job engine';

  async function load(note) {
    try {
      const job = await client.getJob(id);
      if (token !== listToken) return;
      drawJob(job, note);
    } catch (e) {
      if (token !== listToken) return;
      handleCallError(e, (msg) => show(backLink(), banner('error', e instanceof ApiError && e.status === 404 ? 'No such job.' : msg)));
    }
  }

  // perform runs one action. A refusal is shown with the engine's own message, and the job is
  // reloaded so the buttons match what the job really is now (ADR 010).
  async function perform(call, successText, controls) {
    controls.forEach((c) => { c.disabled = true; });
    try {
      const job = await call();
      if (token !== listToken) return;
      drawJob(job, banner('success', successText(job)));
    } catch (e) {
      if (token !== listToken) return;
      handleCallError(e, async (msg) => {
        try {
          const fresh = await client.getJob(id);
          if (token === listToken) drawJob(fresh, banner('error', msg));
        } catch (e2) {
          handleCallError(e2, (m2) => show(backLink(), banner('error', msg), banner('error', m2)));
        }
      });
    }
  }

  function drawJob(job, note) {
    const refresh = el('button', { type: 'button', text: 'Refresh', onclick: () => load() });
    const errors = job.errors.length === 0
      ? el('p', { class: 'muted', text: 'No errors recorded.' })
      : el('div', { class: 'table-wrap' }, el('table', {},
        el('thead', {}, el('tr', {}, ['Attempt', 'Kind', 'Message', 'When'].map((h) => el('th', { scope: 'col', text: h })))),
        el('tbody', {}, job.errors.map((e) => el('tr', {},
          el('td', { text: e.attempt }), el('td', { text: e.kind }),
          el('td', { class: 'mono', text: e.message }), el('td', { text: formatTime(e.occurred_at) }))))));

    show(
      backLink(),
      note,
      el('div', { class: 'toolbar' },
        el('h2', { class: 'mono', text: job.id }), badge(job.state),
        isEscalated(job) ? el('span', { class: 'badge escalated', text: 'escalated' }) : null,
        el('span', { class: 'spacer' }), refresh),
      el('section', { class: 'card' }, el('h2', { text: 'Job' }), facts([
        ['Type', text(job.type)], ['Client', text(job.client_id)],
        ['Attempts', job.attempt + ' of ' + job.max_attempts],
        ['Run at', formatTime(job.run_at)], ['Created', formatTime(job.created_at)],
        ['Updated', formatTime(job.updated_at)], ['Finished', formatTime(job.finished_at)],
        ['Lease owner', text(job.lease_owner)], ['Lease expires', formatTime(job.lease_expires_at)],
        ['Approval deadline', formatTime(job.approval_deadline)], ['Escalated at', formatTime(job.escalated_at)],
        ['Decided by', text(job.decided_by)], ['Decided at', formatTime(job.decided_at)],
        ['Decision reason', text(job.decision_reason)],
        ['Redriven by', text(job.redriven_by)], ['Redriven at', formatTime(job.redriven_at)],
      ])),
      el('section', { class: 'card' }, el('h2', { text: 'Payload' }), el('pre', { text: prettyJson(job.payload) })),
      el('section', { class: 'card' }, el('h2', { text: 'Result' }), el('pre', { text: prettyJson(job.result) })),
      el('section', { class: 'card' }, el('h2', { text: 'Error history' }), errors),
      actionsCard(job));
  }

  function actionsCard(job) {
    const actions = jobActions(job);
    if (actions.length === 0) {
      return el('section', { class: 'card' }, el('h2', { text: 'Actions' }),
        el('p', { class: 'muted', text: 'No actions are available for a ' + job.state.replace('_', ' ') + ' job.' }));
    }
    const panels = [];
    if (actions.includes('approve')) panels.push(approvePanel(job), rejectPanel(job));
    if (actions.includes('redrive')) panels.push(redrivePanel(job));
    return el('section', { class: 'card' }, el('h2', { text: 'Actions' }), el('div', { class: 'actions' }, panels));
  }

  function approvePanel(job) {
    const reason = el('textarea', { id: 'approve-reason', maxlength: MAX_REASON_CHARS });
    const err = el('div', { class: 'field-error' });
    const button = el('button', { class: 'primary', type: 'button', text: 'Approve' });
    button.addEventListener('click', () => {
      const v = validateReason(reason.value, { required: false });
      err.textContent = v.ok ? '' : v.error;
      if (!v.ok) return;
      perform(() => client.approve(job.id, v.value || undefined), () => 'Approved. The job is queued.', allButtons());
    });
    return el('div', { class: 'action' }, el('h3', { text: 'Approve' }),
      el('div', { class: 'field' }, el('label', { for: 'approve-reason', text: 'Reason (optional)' }), reason, err), button);
  }

  function rejectPanel(job) {
    const reason = el('textarea', { id: 'reject-reason', maxlength: MAX_REASON_CHARS });
    const err = el('div', { class: 'field-error' });
    const button = el('button', { class: 'danger', type: 'button', text: 'Reject' });
    button.addEventListener('click', () => {
      const v = validateReason(reason.value, { required: true });
      err.textContent = v.ok ? '' : v.error;
      if (!v.ok) return;
      perform(() => client.reject(job.id, v.value), () => 'Rejected. This is final.', allButtons());
    });
    return el('div', { class: 'action' }, el('h3', { text: 'Reject' }),
      el('div', { class: 'field' }, el('label', { for: 'reject-reason', text: 'Reason (required)' }), reason, err), button);
  }

  function redrivePanel(job) {
    const input = el('input', { id: 'redrive-max', type: 'text', inputmode: 'numeric', autocomplete: 'off' });
    const err = el('div', { class: 'field-error' });
    const button = el('button', { class: 'primary', type: 'button', text: 'Redrive' });
    button.addEventListener('click', () => {
      const v = validateRedrive(input.value, job.attempt);
      err.textContent = v.ok ? '' : v.error;
      if (!v.ok) return;
      perform(() => client.redrive(job.id, v.value), (j) => 'Redriven with ' + j.max_attempts + ' attempts. The job is queued.', allButtons());
    });
    return el('div', { class: 'action' }, el('h3', { text: 'Redrive' }),
      el('div', { class: 'field' }, el('label', { for: 'redrive-max', text: 'New max attempts' }), input,
        el('div', { class: 'hint', text: redriveHint(job.attempt) }), err), button);
  }

  const allButtons = () => [...viewEl.querySelectorAll('.actions button')];

  load();
}

function backLink() {
  return el('p', {}, el('a', { href: jobsHash(DEFAULT_TAB), text: '← Back to the lists' }));
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

function route() {
  if (!me) return; // the sign-in screen stays until a key is accepted
  const r = parseRoute(location.hash);
  if (r.name === 'job') renderJob(r.id);
  else renderList(r.tab);
}

window.addEventListener('hashchange', route);
renderSignIn(null);
