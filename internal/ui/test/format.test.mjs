// Tests of the operator page's pure helpers: tabs, routes, what to show when a call is refused,
// and the checks on what the operator types. Run with: node --test internal/ui/test/*.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { ApiError } from '../web/client.mjs';
import {
  DEFAULT_TAB, SLOW_TABS, TABS, describeError, formatTime, isEscalated, jobActions, jobHash, jobsHash,
  lastErrorText, parseRoute, prettyJson, redriveHint, shortId, truncate, validateReason, validateRedrive,
} from '../web/format.mjs';

const ID = '6f1c0e5a-6a53-4d6e-9d57-0a2b7f2f6b11';

test('the tabs are the six states plus Escalated, and the default is Waiting approval', () => {
  assert.deepEqual(TABS.map((t) => t.id),
    ['waiting_approval', 'escalated', 'dead', 'running', 'queued', 'succeeded', 'rejected']);
  assert.equal(DEFAULT_TAB, 'waiting_approval');
  assert.deepEqual(TABS.find((t) => t.id === 'escalated').query, { escalated: true });
});

test('only the lists that are not indexed carry the slow warning', () => {
  assert.deepEqual([...SLOW_TABS].sort(), ['queued', 'rejected', 'running', 'succeeded']);
  assert.ok(!SLOW_TABS.has('waiting_approval') && !SLOW_TABS.has('dead') && !SLOW_TABS.has('escalated'));
});

test('routes round-trip, and anything else falls back to the default list', () => {
  assert.deepEqual(parseRoute(jobHash(ID)), { name: 'job', id: ID });
  assert.deepEqual(parseRoute(jobsHash('dead')), { name: 'jobs', tab: 'dead' });
  for (const bad of ['', '#', '#/', '#/jobs/nonsense', '#/job/not-a-uuid', '#/job/', '#/other', undefined, null]) {
    assert.deepEqual(parseRoute(bad), { name: 'jobs', tab: DEFAULT_TAB }, String(bad));
  }
});

test('a job id with markup in it is not a route', () => {
  assert.deepEqual(parseRoute('#/job/<img src=x onerror=alert(1)>'), { name: 'jobs', tab: DEFAULT_TAB });
});

test('approve and reject belong to waiting_approval; redrive to dead; nothing else has actions', () => {
  assert.deepEqual(jobActions({ state: 'waiting_approval' }), ['approve', 'reject']);
  assert.deepEqual(jobActions({ state: 'dead' }), ['redrive']);
  for (const s of ['queued', 'running', 'succeeded', 'rejected']) assert.deepEqual(jobActions({ state: s }), [], s);
});

test('a reject needs a reason; an approve does not', () => {
  assert.equal(validateReason('', { required: true }).ok, false);
  assert.equal(validateReason('   \n ', { required: true }).ok, false);
  assert.equal(validateReason(undefined, { required: true }).ok, false);
  assert.deepEqual(validateReason('  duplicate  ', { required: true }), { ok: true, value: 'duplicate' });
  assert.deepEqual(validateReason('', { required: false }), { ok: true, value: '' });
});

test('a reason is at most 1000 characters, counted as characters not bytes', () => {
  assert.equal(validateReason('x'.repeat(1000), { required: true }).ok, true);
  assert.equal(validateReason('x'.repeat(1001), { required: true }).ok, false);
  assert.equal(validateReason('é'.repeat(1000), { required: true }).ok, true);
  assert.equal(validateReason('😀'.repeat(1000), { required: true }).ok, true);
});

test('a redrive needs a whole number above the attempts made, at most 100', () => {
  assert.equal(validateRedrive('', 3).ok, false);
  assert.equal(validateRedrive('abc', 3).ok, false);
  assert.equal(validateRedrive('5.5', 3).ok, false);
  assert.equal(validateRedrive('-4', 3).ok, false);
  assert.equal(validateRedrive('1e2', 3).ok, false);
  assert.equal(validateRedrive('3', 3).ok, false, 'equal to attempts');
  assert.equal(validateRedrive('2', 3).ok, false, 'below attempts');
  assert.equal(validateRedrive('101', 3).ok, false);
  assert.deepEqual(validateRedrive('4', 3), { ok: true, value: 4 });
  assert.deepEqual(validateRedrive(' 100 ', 3), { ok: true, value: 100 });
  assert.match(validateRedrive('3', 3).error, /greater than 3/);
});

test('the redrive hint states the allowed range', () => {
  assert.equal(redriveHint(8), 'More than 8, at most 100.');
});

test('a refusal shows the engine\'s message and its details', () => {
  const e = new ApiError(409, 'not_waiting_approval', 'The job is not waiting for approval.', { state: 'rejected', decided_by: 'op1' });
  assert.equal(describeError(e), 'The job is not waiting for approval. (state: rejected, decided_by: op1)');
  const e2 = new ApiError(400, 'invalid_request', 'A reject needs a reason.');
  assert.equal(describeError(e2), 'A reject needs a reason.');
});

test('no answer and a rejected key get their own text', () => {
  assert.match(describeError(new ApiError(0, 'network', 'x')), /Could not reach the engine/);
  assert.match(describeError(new ApiError(401, 'unauthorized', 'x')), /did not accept this key/);
});

test('times are shown in UTC to the second, and a missing time is a dash', () => {
  assert.equal(formatTime('2026-10-16T03:25:53.004Z'), '2026-10-16 03:25:53 UTC');
  assert.equal(formatTime('2026-10-16T03:25:53-06:00'), '2026-10-16 09:25:53 UTC');
  assert.equal(formatTime(null), '—');
  assert.equal(formatTime(undefined), '—');
  assert.equal(formatTime('not a time'), 'not a time');
});

test('small display helpers', () => {
  assert.equal(shortId(ID), '6f1c0e5a');
  assert.equal(truncate('abcdef', 4), 'abc…');
  assert.equal(truncate('abc', 4), 'abc');
  assert.equal(prettyJson(null), '—');
  assert.equal(prettyJson({ a: 1 }), '{\n  "a": 1\n}');
  assert.equal(prettyJson('text'), '"text"');
  assert.equal(isEscalated({ state: 'waiting_approval', escalated_at: '2026-10-16T00:00:00Z' }), true);
  assert.equal(isEscalated({ state: 'waiting_approval', escalated_at: null }), false);
  assert.equal(isEscalated({ state: 'queued', escalated_at: '2026-10-16T00:00:00Z' }), false);
  assert.equal(lastErrorText({ last_error: null }), '—');
  assert.equal(lastErrorText({ last_error: { kind: 'retryable', message: 'boom' } }), 'retryable: boom');
});
