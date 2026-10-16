# The operator page

The page is for a person who runs the system (an operator). It lists jobs by state, shows one job in full, and approves, rejects or redrives it. What it does is in [ADR 010](adr/010-ui-scope.md). How it is built and why is in [ADR 015](adr/015-operator-page-build.md).

## Using it

1. Create an operator key (it is printed once): `go run ./cmd/jobkeys create -name ops-alice -role operator`. Use one key per person, because the key's name is what gets recorded as `decided_by` and in the redrive record.
2. Start the engine and open `http://localhost:8080/ui/`.
3. Paste the key. The header then reads "acting as ops-alice (operator)". The page refuses a key that is not an operator key, and a revoked or unknown key.

| Screen | What it does |
|---|---|
| **Waiting approval** (opens here) | Jobs waiting for a decision, with the approval deadline. An escalated job carries a red "escalated" tag, and also has its own **Escalated** tab |
| **Dead** | Jobs that ran out of attempts or failed permanently, with the last error in the row |
| **Running, Queued, Succeeded, Rejected** | The other states. These lists are not indexed, so the page warns that they can be slow on a large table |
| **A job** (click its id) | Every field, the payload and result, and the error history. Below that are the actions that apply to its state |

- **Approve** (reason optional) and **Reject** (reason required) are on the screen of a job that is waiting for approval. **Redrive** is on the screen of a dead job and asks for the new maximum number of attempts, which must be more than the attempts already made and at most 100.
- There is no action on a list row. You open the job first, so you see the payload before you decide.
- If another operator got there first, the engine refuses and the page shows its message with the job's current state, for example "The job is not waiting for approval. (decided_by: ops-alice, state: queued)". It then reloads the job.
- Lists do not update by themselves. **Refresh** reloads. A list loads 50 jobs at a time, and **Load more** fetches the next page.
- The key lives in the page's memory. A reload, or **Sign out**, forgets it. A job's address (`/ui/#/job/<id>`) survives a reload, so you sign in again and land on the same job.

## How it is tested

| Layer | What it covers | Run it with |
|---|---|---|
| Node tests (26) | The calls to the engine: method, path, headers and body of each; how an error looks. The text shown for a refusal, the checks on what is typed, the address handling | `node --test internal/ui/test/*.test.mjs` |
| Go tests | The page's files are served with the right types and with the Content-Security-Policy, frame and no-sniff headers on every response. The page's scripts contain no call that turns text into markup. The HTML has nothing inline. `GET /whoami`, `/ui/` without a key, and the redirect from `/` | `go test ./internal/ui ./internal/api` |
| Browser check (48 checks) | Chrome drives every screen against a real engine and database. See below | by hand, as below |

### The browser check

It needs Chrome, Node, Postgres on the PATH, and a server your user can create databases on. Nothing is installed into the repository except what `npm install` puts in `internal/ui/e2e/node_modules` (git ignores it).

```
cd internal/ui/e2e
npm install                              # playwright-core only; it uses the installed Chrome
./setup.sh /tmp/ui-check                 # fresh database, engine on :18090, seed data
node browser-check.mjs /tmp/ui-check     # prints PASS or FAIL for each check
lsof -ti tcp:18090 | xargs kill          # stop the engine afterwards
```

The seed data includes a payload and an error message that would run as code if the page ever treated text as markup. What it checks, in a real browser:

- **Sign-in:** a wrong key and a client key are refused; the key is emptied from the field after use, and is in no storage, cookie, address or page text; a reload asks again; a deep link to a job survives the reload.
- **Lists:** the right jobs on each tab; escalated jobs tagged; the slow-list warning; 50 at a time with Load more and no job repeated.
- **Untrusted text:** the hostile payload, error message and reject reason appear as literal text. No element is created from them and no dialog opens.
- **Actions:** a reject with no reason is refused in the page and nothing is sent. Reject, approve (with and without a reason, and on an escalated job) and redrive each change the job, and the database agrees with what the page shows, including `decided_by` and `redriven_by`. Bad redrive values are refused in the page, and nothing is sent.
- **Two operators:** the second one is refused with the engine's message and the job's current state, and the page reloads the job. The database has one decision.
- **A key revoked while the page is open:** the next click returns to sign-in with a message.
- **The policy:** an injected inline script is blocked and reported, and a page on another address cannot show the operator page in a frame. A control frame of a page without the header does render, so the block is the policy at work.

## Limits

- **One browser.** Only Chrome 155 (headless) was used. Safari and Firefox were not tried.
- **Looks were judged from screenshots at one desktop width, in light and dark mode.** Narrow (phone) widths were not checked. No accessibility audit or screen-reader test was done, though fields have labels and messages are announced.
- **The key travels over plain HTTP** unless a TLS proxy is in front. The sign-in screen says so.
- **The key sits in the page's memory.** A browser extension that can read the page could read it. The page loads nothing from other sites, and the policy forbids it.
- **No live updates, no bulk actions and no job submission**, as ADR 010 says.
- **Framing is blocked by headers.** An old browser that ignores both `frame-ancestors` and `X-Frame-Options` would not be protected. None was tried.
