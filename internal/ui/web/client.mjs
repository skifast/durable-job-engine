// The operator page's API client. It knows the endpoints in docs/api.md and nothing about
// the page, so it can be tested without a browser. The key lives in a variable inside
// createClient and nowhere else: not in storage, not in the URL (ADR 010).

// ApiError is any answer that is not a success, or no answer at all (status 0).
export class ApiError extends Error {
  constructor(status, code, message, details) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details || {};
  }
}

// createClient returns the calls. fetchFn is injected so tests can fake the network.
export function createClient(fetchFn = (...args) => fetch(...args)) {
  let key = null;

  async function call(method, path, body) {
    const headers = { Authorization: 'Bearer ' + key };
    const init = { method, headers };
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    let res;
    try {
      res = await fetchFn(path, init);
    } catch (e) {
      throw new ApiError(0, 'network', 'Could not reach the engine.');
    }
    let data = null;
    try {
      const text = await res.text();
      data = text ? JSON.parse(text) : null;
    } catch (e) {
      data = null; // an answer that is not JSON; handled below
    }
    if (!res.ok) {
      const err = data && data.error ? data.error : {};
      throw new ApiError(res.status, err.code || 'unknown',
        err.message || 'The engine answered ' + res.status + '.', err.details);
    }
    return data;
  }

  const jobPath = (id) => '/jobs/' + encodeURIComponent(id);

  return {
    setKey(k) { key = k || null; },
    hasKey() { return key !== null; },

    // GET /whoami
    whoami() { return call('GET', '/whoami'); },

    // GET /jobs. query: { state, escalated, cursor, limit }. Only the fields given are sent.
    listJobs(query = {}) {
      const p = new URLSearchParams();
      if (query.state) p.set('state', query.state);
      if (query.escalated) p.set('escalated', 'true');
      if (query.cursor) p.set('cursor', query.cursor);
      if (query.limit) p.set('limit', String(query.limit));
      const qs = p.toString();
      return call('GET', '/jobs' + (qs ? '?' + qs : ''));
    },

    // GET /jobs/{id}
    getJob(id) { return call('GET', jobPath(id)); },

    // POST /jobs/{id}/approve. The reason is optional; with none the body is left out.
    approve(id, reason) {
      return call('POST', jobPath(id) + '/approve', reason ? { reason } : undefined);
    },

    // POST /jobs/{id}/reject. The reason is required (the API refuses without it).
    reject(id, reason) { return call('POST', jobPath(id) + '/reject', { reason }); },

    // POST /jobs/{id}/redrive. maxAttempts is a whole number.
    redrive(id, maxAttempts) {
      return call('POST', jobPath(id) + '/redrive', { max_attempts: maxAttempts });
    },
  };
}
