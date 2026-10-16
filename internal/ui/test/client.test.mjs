// Tests of the operator page's API client: the right method, path, headers and body for each
// call, and what an error looks like. Run with: node --test internal/ui/test/*.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { ApiError, createClient } from '../web/client.mjs';

// fakeFetch records the request and answers with what the test says.
function fakeFetch(status, body) {
  const calls = [];
  const fn = async (path, init) => {
    calls.push({ path, ...init });
    return {
      ok: status >= 200 && status < 300,
      status,
      text: async () => (body === undefined ? '' : typeof body === 'string' ? body : JSON.stringify(body)),
    };
  };
  fn.calls = calls;
  return fn;
}

function clientWith(status, body) {
  const f = fakeFetch(status, body);
  const c = createClient(f);
  c.setKey('secret-key');
  return { c, f };
}

const ID = '6f1c0e5a-6a53-4d6e-9d57-0a2b7f2f6b11';

test('every call sends the key as a Bearer token', async () => {
  const { c, f } = clientWith(200, { name: 'ops', role: 'operator' });
  await c.whoami();
  assert.equal(f.calls[0].headers.Authorization, 'Bearer secret-key');
  assert.equal(f.calls[0].path, '/whoami');
  assert.equal(f.calls[0].method, 'GET');
});

test('the key is not in the path, the query or the body of any call', async () => {
  const { c, f } = clientWith(200, { jobs: [], next_cursor: null });
  await c.listJobs({ state: 'dead', cursor: 'abc', limit: 50 });
  await c.approve(ID, 'ok');
  await c.reject(ID, 'no');
  await c.redrive(ID, 5);
  for (const call of f.calls) {
    assert.ok(!call.path.includes('secret-key'));
    assert.ok(!(call.body || '').includes('secret-key'));
  }
});

test('listJobs builds the query from the fields given, and nothing else', async () => {
  const { c, f } = clientWith(200, { jobs: [], next_cursor: null });
  await c.listJobs();
  await c.listJobs({ state: 'waiting_approval' });
  await c.listJobs({ escalated: true });
  await c.listJobs({ state: 'dead', cursor: 'c1', limit: 50 });
  assert.deepEqual(f.calls.map((x) => x.path), [
    '/jobs',
    '/jobs?state=waiting_approval',
    '/jobs?escalated=true',
    '/jobs?state=dead&cursor=c1&limit=50',
  ]);
  assert.ok(f.calls.every((x) => x.method === 'GET' && x.body === undefined));
});

test('listJobs encodes a cursor safely', async () => {
  const { c, f } = clientWith(200, { jobs: [], next_cursor: null });
  await c.listJobs({ cursor: 'a b&c=d' });
  assert.equal(f.calls[0].path, '/jobs?cursor=a+b%26c%3Dd');
});

test('getJob reads one job', async () => {
  const { c, f } = clientWith(200, { id: ID });
  const job = await c.getJob(ID);
  assert.equal(job.id, ID);
  assert.equal(f.calls[0].path, '/jobs/' + ID);
  assert.equal(f.calls[0].method, 'GET');
});

test('a job id is encoded in the path', async () => {
  const { c, f } = clientWith(200, {});
  await c.getJob('../x?y');
  assert.equal(f.calls[0].path, '/jobs/..%2Fx%3Fy');
});

test('approve posts to /approve; the reason is optional', async () => {
  const { c, f } = clientWith(200, { id: ID, state: 'queued' });
  await c.approve(ID);
  await c.approve(ID, 'looks fine');
  assert.equal(f.calls[0].path, `/jobs/${ID}/approve`);
  assert.equal(f.calls[0].method, 'POST');
  assert.equal(f.calls[0].body, undefined);
  assert.deepEqual(JSON.parse(f.calls[1].body), { reason: 'looks fine' });
  assert.equal(f.calls[1].headers['Content-Type'], 'application/json');
});

test('reject posts the reason to /reject', async () => {
  const { c, f } = clientWith(200, { id: ID, state: 'rejected' });
  await c.reject(ID, 'duplicate');
  assert.equal(f.calls[0].path, `/jobs/${ID}/reject`);
  assert.equal(f.calls[0].method, 'POST');
  assert.deepEqual(JSON.parse(f.calls[0].body), { reason: 'duplicate' });
});

test('redrive posts max_attempts as a number to /redrive', async () => {
  const { c, f } = clientWith(200, { id: ID, state: 'queued' });
  await c.redrive(ID, 12);
  assert.equal(f.calls[0].path, `/jobs/${ID}/redrive`);
  assert.equal(f.calls[0].method, 'POST');
  assert.deepEqual(JSON.parse(f.calls[0].body), { max_attempts: 12 });
});

test('a refusal becomes an ApiError with the engine\'s code, message and details', async () => {
  const { c } = clientWith(409, {
    error: { code: 'not_waiting_approval', message: 'The job is not waiting for approval.', details: { state: 'rejected', decided_by: 'op1' } },
  });
  await assert.rejects(c.approve(ID), (e) => {
    assert.ok(e instanceof ApiError);
    assert.equal(e.status, 409);
    assert.equal(e.code, 'not_waiting_approval');
    assert.equal(e.message, 'The job is not waiting for approval.');
    assert.deepEqual(e.details, { state: 'rejected', decided_by: 'op1' });
    return true;
  });
});

test('an answer that is not the engine\'s error shape still gives an ApiError', async () => {
  const { c } = clientWith(502, '<html>bad gateway</html>');
  await assert.rejects(c.getJob(ID), (e) => e instanceof ApiError && e.status === 502 && e.code === 'unknown');
});

test('no answer at all is status 0', async () => {
  const c = createClient(async () => { throw new TypeError('failed to fetch'); });
  c.setKey('k');
  await assert.rejects(c.whoami(), (e) => e instanceof ApiError && e.status === 0 && e.code === 'network');
});

test('setKey(null) forgets the key', () => {
  const c = createClient(fakeFetch(200, {}));
  c.setKey('k');
  assert.equal(c.hasKey(), true);
  c.setKey(null);
  assert.equal(c.hasKey(), false);
});
