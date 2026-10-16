# ADR 015: How the operator page is built and served

**Status:** Accepted (2026-10-16)  
**Date:** 2026-10-16  
**Spec sections:** 7 (operator page), 12 (test plan)  
**Depends on:** ADR 010 (what the page does), ADR 013 (operator API keys)  
**Changes:** the API gains `GET /whoami` and the engine serves static files under `/ui/` (docs/api.md)  

## Context

ADR 010 decided what the operator page does and left two things to the build phase: which framework, and how the page is served. Terms: **static files** are HTML, CSS and JavaScript that a server hands over unchanged. **CORS** is a browser rule that stops a page from calling a server at a different address unless that server opts in. **XSS** (cross-site scripting) is text written by an attacker that ends up running as code in a page.

## Decisions

### 1. The engine serves the page, from its own port

The page's files are compiled into the engine binary (Go's `go:embed`) and served under `/ui/`. The page and the API share one address, so there is no CORS and nothing extra to deploy. `GET /` redirects to `/ui/`. The `/ui/` routes need no API key: the files hold no data, and every API call the page makes still needs the operator key.

Considered and not chosen: a separate server for the page. It would need CORS turned on in the API, which opens a security setting to solve a problem the chosen design does not have.

### 2. Plain HTML, CSS and JavaScript, no framework and no build step

The page is a few small files that run in the browser as written. Nothing is installed to build it, and the whole page can be read from top to bottom. The JavaScript is split into modules: the part that talks to the API and the part that formats and checks input (`client.mjs`, `format.mjs`) do not touch the page, so they can be tested with Node's built-in test runner. Only `app.mjs` builds screens.

Considered and not chosen: React or similar with a build tool. It adds a toolchain and many dependencies to a backend project for a page of five screens.

### 3. Safety rules for the page

- **Everything from a job is shown as plain text.** Payloads, results and error messages are written by clients and handlers, so they are untrusted. The page builds its screens with `textContent` and never with `innerHTML` or anything that reads text as HTML. A test fails if the page's JavaScript contains one of those.
- **A Content-Security-Policy header** on every `/ui/` response lets the page load scripts and styles only from its own address, and call only its own address. The HTML has no inline script, no inline style and no event-handler attributes, because the policy forbids them.
- **The page cannot be shown inside another site's frame** (`frame-ancestors 'none'` and `X-Frame-Options: DENY`). Otherwise another site could place the Approve button under a click.
- The key lives in a JavaScript variable, as ADR 010 says: never in storage, never in the URL.

### 4. `GET /whoami`

Any valid key (client, worker or operator) gets back `{ "name": "...", "role": "..." }`. The page uses it at sign-in. It shows "acting as NAME (operator)", because the key's name is what gets recorded as `decided_by`. It refuses a key whose role is not operator, instead of letting the first click return a 403.

Considered and not chosen: no new call. The page would learn who you are only after an action succeeded, and a non-operator key would sign in and then fail on the first click.

### 5. Actions only on the detail screen

Approve, reject and redrive are on a job's detail screen, below its payload, result and error history. List rows only link to the detail. Approving a payment that was not read is the mistake this page should make hard, and one extra click is the price.

Considered and not chosen: quick buttons on list rows (blind approval in one click).

## Defaults set with these decisions

- The list has a tab per state and opens on "Waiting approval", with an "Escalated" tab. Jobs load with a "Load more" button using the API's cursor. A tab for a list that is not indexed (queued, running, succeeded, rejected) shows a note that it may be slow on a large table (docs/api.md).
- A job is at `/ui/#/job/<id>` and a list at `/ui/#/jobs/<tab>`, so back and reload work.
- The redrive field starts empty and shows the allowed range (more than the job's attempt, at most 100).
- After any action the page shows the API's refusal message if there was one, reloads the job and redraws the buttons. Buttons are disabled while a request is in flight. A 401 from any call (for example a revoked key) returns to the sign-in screen.
- Times are shown in UTC.

## Consequences

- Anyone who can reach the engine can load the page, but cannot do anything on it without an operator key.
- The engine speaks plain HTTP, so on a network the key crosses it in the clear unless a TLS proxy is in front (ADR 013). The README already says so; the page says it too on the sign-in screen.
- The engine binary now carries the page. A page change needs a rebuild of the engine.
- No live updates (ADR 010). Two operators can act on the same job, and the second gets the API's refusal message.
- Node tests cover the API calls and the text the page shows, and Go tests cover the served files and their headers. Neither draws the page. A script in `internal/ui/e2e` drives Chrome through every screen against a real engine and database. It is run by hand, not by `go test`, and it needs Chrome and an npm install (docs/operator-page.md).

## Revisit this decision if

- The page grows past what can be read comfortably as plain modules (many more screens, shared state).
- The page needs to be hosted apart from the engine (then CORS and a policy for it come back).
