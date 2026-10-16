// A check of the operator page in a real browser (Chrome), against a real engine and database.
// Not part of `go test`: run it by hand with ./setup.sh and node. See docs/operator-page.md.
import { chromium } from 'playwright-core';
import { readFileSync } from 'node:fs';
import { execSync } from 'node:child_process';
import http from 'node:http';

const D = process.argv[2];
const BASE = 'http://127.0.0.1:18090';
const [CLIENT, WORKER, ALICE, BOB] = readFileSync(`${D}/keys`, 'utf8').trim().split('\n');
const id = (n) => readFileSync(`${D}/id_${n}`, 'utf8').trim();
const psql = (sql) => execSync(`psql -d jobengine_ui -tA -c "${sql.replace(/"/g, '\\"')}"`).toString().trim();

const results = [];
function check(name, ok, detail = '') {
  results.push({ name, ok });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? '\n        ' + detail : ''}`);
}

const browser = await chromium.launch({ channel: 'chrome', headless: true });

async function newPage(opts = {}) {
  const ctx = await browser.newContext({ viewport: { width: 1200, height: 900 }, ...opts });
  const page = await ctx.newPage();
  page.dialogs = [];
  page.consoleErrors = [];
  page.requests = [];
  page.failed = [];
  page.on('dialog', async (d) => { page.dialogs.push(d.message()); await d.dismiss(); });
  page.on('console', (m) => { if (m.type() === 'error') page.consoleErrors.push(m.text()); });
  page.on('response', (r) => { if (r.status() >= 400 && r.url().startsWith(BASE)) page.failed.push(r.status() + ' ' + new URL(r.url()).pathname); });
  page.on('request', (r) => { if (r.url().startsWith(BASE) && !r.url().includes('/ui/')) page.requests.push(`${r.method()} ${new URL(r.url()).pathname}${new URL(r.url()).search}`); });
  return page;
}
async function signIn(page, key, hash = '') {
  await page.goto(`${BASE}/ui/${hash}`);
  await page.fill('#key', key);
  await page.click('button:has-text("Sign in")');
}
const shot = (page, name) => page.screenshot({ path: `${D}/shots/${name}.png`, fullPage: true });
const rows = (page) => page.locator('tbody tr').count();
// Playwright's waitForFunction evaluates a string in the page, which the page's CSP forbids (and should).
async function poll(fn, timeout = 5000) {
  const t = Date.now();
  while (Date.now() - t < timeout) { if (await fn()) return true; await new Promise((r) => setTimeout(r, 50)); }
  return false;
}

// ---- 1. Entry, sign-in, and what is kept ----
{
  const page = await newPage();
  await page.goto(BASE + '/');
  check('GET / lands on /ui/', new URL(page.url()).pathname === '/ui/', page.url());
  await shot(page, '01-signin');

  await page.fill('#key', 'definitely-not-a-key');
  await page.click('button:has-text("Sign in")');
  await page.waitForSelector('.banner.error');
  check('a wrong key is refused with a message', (await page.textContent('.banner.error')).includes('did not accept this key'), await page.textContent('.banner.error'));
  check('the key field is emptied after a try', (await page.inputValue('#key')) === '');

  await page.fill('#key', CLIENT);
  await page.click('button:has-text("Sign in")');
  await page.waitForSelector('.banner.error:has-text("client key")');
  check('a client key is refused: the page needs an operator key', true, await page.textContent('.banner.error'));

  await page.fill('#key', ALICE);
  await page.click('button:has-text("Sign in")');
  await page.waitForSelector('#who strong');
  check('the header says who is acting', (await page.textContent('#who')).includes('ops-alice') && (await page.textContent('#who')).includes('operator'), (await page.textContent('#who')).trim());
  await page.waitForSelector('tbody tr');
  await shot(page, '02-waiting');

  const leak = await page.evaluate((k) => ({
    storage: JSON.stringify(localStorage) + JSON.stringify(sessionStorage) + document.cookie,
    url: location.href,
    dom: document.documentElement.outerHTML,
  }), ALICE);
  check('the key is not in storage, cookies, the URL or the page', ![leak.storage, leak.url, leak.dom].some((s) => s.includes(ALICE)));

  // ---- 2. Lists ----
  check('Waiting approval lists the 5 waiting jobs', (await rows(page)) === 5, `rows: ${await rows(page)}`);
  const escRow = await page.locator('tbody tr', { hasText: 'escalated' }).count();
  check('the escalated job carries an escalated badge', escRow === 1);

  await page.click('nav.tabs >> text=Escalated');
  await poll(async () => (await rows(page)) === 1);
  check('Escalated tab lists just the escalated job', (await rows(page)) === 1);

  await page.click('nav.tabs >> text=Dead');
  // The old list stays on screen until the address change is handled, so wait for the new rows.
  await poll(async () => (await page.locator('tbody').textContent()).includes('one_try'));
  const deadText = await page.textContent('tbody');
  check('Dead tab shows the error text literally, as text', deadText.includes('<script>alert(1)</script>'), deadText.trim().slice(0, 120));
  check('...and no element was created from it', (await page.locator('main script, main img').count()) === 0 && page.dialogs.length === 0);
  await shot(page, '03-dead');

  await page.click('nav.tabs >> text=Queued');
  await page.waitForSelector('.banner.info');
  check('an unindexed list warns that it can be slow', (await page.textContent('.banner.info')).includes('can be slow'));

  // CSP is really enforced: an injected inline script must not run.
  const ran = await page.evaluate(async () => {
    const s = document.createElement('script');
    s.textContent = 'window.__injected = true';
    document.body.append(s);
    await new Promise((r) => setTimeout(r, 100));
    return window.__injected === true;
  });
  check('the browser blocks an injected inline script (CSP is enforced)', !ran);
  check('...and reports the violation', page.consoleErrors.some((e) => /Content Security Policy/i.test(e)));

  // ---- 3. Reload forgets the key; a deep link survives it ----
  await page.goto(`${BASE}/ui/#/job/${id('wire')}`);
  await page.reload();
  check('a reload asks for the key again', (await page.locator('#key').count()) === 1);
  check('...and the address still names the job', page.url().endsWith(`#/job/${id('wire')}`));
  await page.fill('#key', ALICE);
  await page.click('button:has-text("Sign in")');
  await page.waitForSelector('h2.mono');
  check('signing in lands on the job from the address', (await page.textContent('h2.mono')) === id('wire'));

  // ---- 4. The job screen: untrusted payload is shown as text ----
  const pre = await page.locator('pre').first().textContent();
  check('the payload is shown as text', pre.includes('<img src=x onerror=alert(1)>') && pre.includes('ACME Ltd'));
  check('...no element was made from it and no dialog opened', (await page.locator('main img').count()) === 0 && page.dialogs.length === 0);
  await shot(page, '04-job-waiting');

  // ---- 5. Reject: a reason is required, and nothing is sent without one ----
  const before = page.requests.length;
  await page.click('button:has-text("Reject")');
  check('rejecting with no reason shows an inline error', (await page.textContent('#reject-reason + .field-error')).includes('needs a reason'));
  check('...and sends nothing to the engine', page.requests.length === before);
  await page.fill('#reject-reason', 'wrong beneficiary <b>bold</b>');
  await page.click('button:has-text("Reject")');
  await page.waitForSelector('.banner.success');
  check('rejecting with a reason succeeds', (await page.textContent('.banner.success')).includes('Rejected'));
  check('the job now shows rejected, decided by the key\'s name, with the reason as text',
    (await page.textContent('main')).includes('wrong beneficiary <b>bold</b>') && (await page.textContent('.badge.rejected')) === 'rejected');
  check('...and no actions remain', (await page.locator('.actions button').count()) === 0 && (await page.textContent('main')).includes('No actions are available'));
  check('the database agrees', psql(`select state||'/'||decided_by from jobs where id='${id('wire')}'`) === 'rejected/ops-alice');
  await shot(page, '05-rejected');

  // ---- 6. Approve ----
  await page.goto(`${BASE}/ui/#/job/${id('g1')}`);
  await page.waitForSelector('h2.mono');
  await page.fill('#approve-reason', 'checked with finance');
  await page.click('button:has-text("Approve")');
  await page.waitForSelector('.banner.success');
  check('approving queues the job', (await page.textContent('.badge.queued')) === 'queued');
  check('the database agrees', psql(`select state||'/'||decided_by||'/'||decision_reason from jobs where id='${id('g1')}'`) === 'queued/ops-alice/checked with finance');

  // approve with no reason at all
  await page.goto(`${BASE}/ui/#/job/${id('g2')}`);
  await page.waitForSelector('button:has-text("Approve")');
  await page.click('button:has-text("Approve")');
  await page.waitForSelector('.banner.success');
  check('approving with no reason works', psql(`select state||'/'||coalesce(decision_reason,'none') from jobs where id='${id('g2')}'`) === 'queued/none');

  // an escalated job can still be approved
  await page.goto(`${BASE}/ui/#/job/${id('esc')}`);
  await page.waitForSelector('.badge.escalated');
  check('an escalated job shows the badge on its own screen', true);
  await page.click('button:has-text("Approve")');
  await page.waitForSelector('.banner.success');
  check('an escalated job can still be approved', psql(`select state from jobs where id='${id('esc')}'`) === 'queued');

  // ---- 7. Redrive ----
  await page.goto(`${BASE}/ui/#/job/${id('dead')}`);
  await page.waitForSelector('#redrive-max');
  check('the redrive field shows the allowed range', (await page.textContent('.hint')).includes('More than 1, at most 100'));
  const before2 = page.requests.length;
  for (const bad of ['', 'abc', '1', '101']) {
    await page.fill('#redrive-max', bad);
    await page.click('button:has-text("Redrive")');
    const msg = await page.textContent('#redrive-max ~ .field-error');
    if (!msg) check(`redrive with "${bad}" is refused in the page`, false);
  }
  check('bad redrive values are refused in the page and nothing is sent', page.requests.length === before2);
  await page.fill('#redrive-max', '5');
  await page.click('button:has-text("Redrive")');
  await page.waitForSelector('.banner.success');
  check('a valid redrive queues the job with the new limit', (await page.textContent('.banner.success')).includes('5 attempts') && (await page.textContent('.badge.queued')) === 'queued');
  check('the database agrees', psql(`select state||'/'||max_attempts||'/'||redriven_by from jobs where id='${id('dead')}'`) === 'queued/5/ops-alice');
  await shot(page, '06-redriven');

  // ---- 8. Sign out ----
  await page.click('button:has-text("Sign out")');
  check('signing out returns to the key prompt', (await page.locator('#key').count()) === 1 && (await page.locator('#who strong').count()) === 0);
  // The only refused request expected is the wrong key typed on purpose.
  const unexpected = page.failed.filter((f) => f !== '401 /whoami');
  check('no request failed except the wrong key typed on purpose', unexpected.length === 0, page.failed.join(' | '));
  check('no console errors except the injected-script test', page.consoleErrors.filter((e) => !/Content Security Policy|Failed to load resource/i.test(e)).length === 0, page.consoleErrors.join(' | ').slice(0, 300));
  await page.context().close();
}

// ---- 9. Two operators, one job ----
{
  const a = await newPage();
  const b = await newPage();
  await signIn(a, ALICE, `#/job/${id('race')}`);
  await signIn(b, BOB, `#/job/${id('race')}`);
  await a.waitForSelector('button:has-text("Approve")');
  await b.waitForSelector('button:has-text("Reject")');
  await a.click('button:has-text("Approve")');
  await a.waitForSelector('.banner.success');
  await b.fill('#reject-reason', 'looks wrong to me');
  await b.click('button:has-text("Reject")');
  await b.waitForSelector('.banner.error');
  const msg = await b.textContent('.banner.error');
  check('the second operator is refused with the engine\'s message and the current state', /not waiting for approval/i.test(msg) && msg.includes('queued') && msg.includes('ops-alice'), msg);
  check('...and the page reloaded the job: no buttons left, state shown', (await b.locator('.actions button').count()) === 0 && (await b.textContent('.badge.queued')) === 'queued');
  check('the database has one decision, by the first operator', psql(`select state||'/'||decided_by from jobs where id='${id('race')}'`) === 'queued/ops-alice');
  await shot(b, '07-second-operator-refused');

  await a.context().close();
  await b.context().close();
}
{
  // ---- 10. A key revoked while the page is open ----
  const out = execSync(`${D}/jobkeys create -name ops-carol -role operator`, { env: { ...process.env, DATABASE_URL: `postgres://${process.env.USER}@localhost:5432/jobengine_ui` } }).toString();
  const carol = out.match(/[A-Za-z0-9_-]{40,}/)[0];
  const p = await newPage();
  await signIn(p, carol);
  await p.waitForSelector('#who strong');
  psql(`update api_keys set revoked_at = now() where name = 'ops-carol'`);
  await p.click('button:has-text("Refresh")');
  await p.waitForSelector('#key');
  check('a revoked key sends the page back to sign-in, with a message', (await p.textContent('.banner.error')).includes('did not accept this key'), await p.textContent('.banner.error'));
  check('...and the page holds no key any more', (await p.locator('#who strong').count()) === 0);
  await p.context().close();
}

// ---- 11. Paging and dark mode ----
{
  const subs = [];
  for (let i = 0; i < 60; i++) {
    subs.push(fetch(`${BASE}/jobs`, { method: 'POST', headers: { Authorization: `Bearer ${CLIENT}`, 'Idempotency-Key': `bulk-${i}-${Date.now()}` }, body: JSON.stringify({ type: 'plain', payload: { i } }) }));
  }
  await Promise.all(subs);
  const p = await newPage({ colorScheme: 'dark' });
  await signIn(p, ALICE, '#/jobs/queued');
  await p.waitForSelector('tbody tr');
  check('a list loads 50 at a time', (await rows(p)) === 50, `rows: ${await rows(p)}`);
  await p.click('button:has-text("Load more")');
  await poll(async () => (await rows(p)) > 50);
  const n = await rows(p);
  check('Load more adds the rest, with none repeated', n >= 61 && new Set(await p.locator('tbody tr td.mono a').allTextContents()).size === n, `rows: ${n}`);
  check('...and the button goes away at the end', (await p.locator('button:has-text("Load more"):visible').count()) === 0);
  await shot(p, '08-queued-dark');
  await p.context().close();
}

// ---- 12. Another site cannot put the page in a frame ----
{
  // "Another site" is a page served from a different port. (A frame inside about:blank is blocked
  // for every page, so it would prove nothing.)
  const other = http.createServer((req, res) => {
    res.setHeader('Content-Type', 'text/html');
    res.end(`<iframe src="${BASE}/healthz"></iframe><iframe src="${BASE}/ui/"></iframe>`);
  }).listen(18091, '127.0.0.1');
  const p = await newPage();
  await p.goto('http://127.0.0.1:18091/');
  await new Promise((r) => setTimeout(r, 2000));
  const frames = p.frames().filter((f) => f !== p.mainFrame());
  const control = frames.find((f) => f.url().endsWith('/healthz'));
  const controlWorks = control ? (await control.locator('body').textContent()).includes('"status"') : false;
  let sawPage = false;
  for (const f of frames) if (f !== control && (await f.locator('#key').count()) > 0) sawPage = true;
  check('control: a page without the header does render in a frame', controlWorks);
  check('the operator page does not render inside another site\'s frame', !sawPage && controlWorks);
  check('...and the browser says why', p.consoleErrors.some((e) => /frame-ancestors 'none'/.test(e)), p.consoleErrors.join(' | ').slice(0, 160));
  await p.context().close();
  other.close();
}

await browser.close();
const failed = results.filter((r) => !r.ok);
console.log(`\n${results.length - failed.length} of ${results.length} checks passed`);
process.exit(failed.length ? 1 : 0);
