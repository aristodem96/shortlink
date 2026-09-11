# shortlink

A URL shortener with click analytics, written in Go.

It is a small service on purpose. The interesting part is not the feature list —
shortening a URL is a `INSERT` and a `SELECT` — but what sits around it: a cache
that survives a stampede, analytics that never slow the redirect down, a rate
limiter that stays correct under concurrency, and a shutdown that loses nothing.

```
POST /api/v1/links  {"target_url": "https://go.dev/blog/context"}
  -> 201  {"code": "i8zjvlJ", "short_url": "http://localhost:8080/i8zjvlJ"}

GET /i8zjvlJ
  -> 302  Location: https://go.dev/blog/context
```

---

## Contents

- [Quick start](#quick-start)
- [Deploying to a server](#deploying-to-a-server)
- [Architecture](#architecture)
- [Design decisions](#design-decisions)
- [API](#api)
- [Selling access through Telegram](#selling-access-through-telegram)
- [Configuration](#configuration)
- [Testing](#testing)
- [Out of scope](#out-of-scope)

---

## Quick start

Requires Docker. Nothing else — Go, Postgres and Redis all live in containers.

```bash
make up                  # postgres + redis + api, migrations run on start
make key NAME=local      # prints an API key once; store it
```

```bash
KEY=sl_...               # the key from the previous step

# shorten a URL
curl -X POST localhost:8080/api/v1/links \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"target_url":"https://go.dev/blog/context","alias":"go-ctx"}'

# follow it
curl -i localhost:8080/go-ctx

# see the clicks
curl localhost:8080/api/v1/links/go-ctx/stats -H "Authorization: Bearer $KEY"
```

Interactive API docs are at <http://localhost:8080/docs>, metrics at
`/metrics`, health at `/healthz` and `/readyz`. `make help` lists every target.

### Running it locally

Docker for the dependencies, `go run` for the service — a code change takes a
restart instead of an image build.

```bash
make deps-up             # just postgres + redis
cp .env.example .env     # the Makefile exports this into `make run`
make run
```

`.env` is not read by Go itself; the Makefile loads it and puts it in the
environment of every target. Anything already set in your shell wins over the
file, so a one-off override needs no editing:

```bash
HTTP_ADDR=:8090 BASE_URL=http://localhost:8090 make run
```

### When port 8080 is taken

Both stacks default to 8080, which is a popular port. Find the squatter and
pick another:

```bash
lsof -nP -iTCP:8080 -sTCP:LISTEN        # who has it
docker ps --format '{{.Names}}\t{{.Ports}}'   # often another project's container

API_PORT=18080 make up                  # the Docker stack elsewhere
HTTP_ADDR=:8090 make run                # the local process elsewhere
```

The service refuses to start rather than half-work, and says exactly what
failed:

```
fatal: http server: listen on :8080: bind: address already in use
```

### Other startup failures

| Message | Cause | Fix |
|---|---|---|
| `IP_HASH_SALT must be set` | no `.env`, and the salt has no default on purpose | `cp .env.example .env` |
| `postgres: ping: ... connection refused` | dependencies are not up | `make deps-up` |
| `Cannot connect to the Docker daemon` | Docker Desktop is not running | start Docker, wait for the whale |
| `failed to set up container networking ... port is already allocated` | 8080 taken by another container | `API_PORT=18080 make up` |

Configuration is validated at startup and reports **every** problem at once, so
a misconfigured deployment does not fail one variable per restart.

### Deploying to a server

`docker-compose.prod.yml` is an overlay, not a replacement: it layers on top of
the base file, so the local workflow above keeps working untouched.

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build
```

What it changes, and why each one matters on a public IP:

| Base file (local) | Overlay (server) | Reason |
|---|---|---|
| Postgres on `0.0.0.0:5432` | not published | the port is scanned continuously; the api reaches it over the private compose network |
| Redis on `0.0.0.0:6379`, no password | not published, password required | an unauthenticated Redis is among the most reliably exploited services on the internet |
| api on `0.0.0.0:8080` | not published | Caddy is the only way in, and it terminates TLS |
| passwords `shortlink:shortlink` | `${VAR:?}`, refuses to start unset | the defaults are published in this repository |
| `IP_HASH_SALT` defaults | required | a known salt makes the visitor hashes reversible |
| direct connections | `TRUST_PROXY_HEADERS=true` | behind a proxy every click would otherwise be attributed to Caddy's own address |
| logs unbounded | 10 MB × 3 per service | container logs fill the disk given enough time |

Caddy obtains and renews the certificate itself — no certbot step, no cron job.
It also answers 404 on `/metrics` and `/readyz`: the first is a process-wide
dump with no per-tenant breakdown, the second reports which dependency is down.
`/healthz` stays public, because an external uptime monitor needs somewhere to
knock and the response carries nothing sensitive. A tenant's own numbers come
from `/api/v1/links/{code}/stats`, which is authenticated and scoped to the key.

**Full instructions, including where the checkout belongs on the server,
backups and troubleshooting: [DEPLOYMENT.md](DEPLOYMENT.md).**

---

## Architecture

Four layers, with dependencies pointing inwards only. `domain` imports nothing
from the project; `main` is the only place that knows every concrete type.

```
                       cmd/api (composition root)
                                 |
             +-------------------+-------------------+
             |                                       |
   transport/httpapi                          storage/postgres
   routing, middleware,                       storage/rediscache
   JSON <-> service                           analytics (collector)
             |                                       |
             +-------------------+-------------------+
                                 |
                            service
                 use cases, cache-aside, ports
                                 |
                             domain
                 entities, validation, errors
```

The ports (`LinkRepository`, `LinkCache`, `ClickRecorder`, `CodeGenerator`,
`APIKeyRepository`) are declared in `internal/service/ports.go` — on the
consumer side, the idiomatic Go direction. Storage packages never import the
service; they just happen to satisfy it. The same applies one layer up:
`httpapi` declares the `LinkService`, `Authenticator` and `RateLimiter`
interfaces it consumes.

**The redirect path**, which is the only one that has to be fast:

```
GET /{code}
  |
  |-- shaped like a code?          no -> 404, nothing touched
  |-- Redis GET link:{code}
  |     hit           -> done
  |     negative hit  -> 404, database not touched
  |     miss          -> singleflight -> Postgres -> populate Redis
  |-- expired?                     yes -> 410
  |-- enqueue click (non-blocking, dropped if the buffer is full)
  '-- 302 Location: target
```

---

## Design decisions

This is the section worth reading. Each of these is a place where the obvious
implementation is wrong in a way that only shows up under load.

### Short codes are random, not sequential

The textbook shortener encodes an auto-incrementing ID in base62. That makes
codes enumerable: anyone can count upwards and walk the entire corpus of links
and their targets. `internal/shortid` draws from `crypto/rand` instead, with
rejection sampling to remove the modulo bias a naive `b % 62` would introduce
(the first eight characters of the alphabet would otherwise appear ~1.6x too
often — there is a test for it).

Random codes can collide, so `Create` retries up to three times on a unique
violation. With a 7-character code the keyspace is 62⁷ ≈ 3.5 × 10¹², so three
consecutive collisions means something else is broken. Collisions are counted in
`shortlink_code_collisions_total`; a rising rate is the signal to raise
`CODE_LENGTH`.

### Cache misses are cached too

A cache that only stores hits has an open door: request a million random codes
and every one is a database read. `SetMissing` stores a sentinel for 30 seconds,
so a scan costs one query instead of a million. `Create` invalidates that entry,
otherwise a code someone probed before it existed would 404 until the negative
entry expired.

### Concurrent misses collapse into one query

When a popular link falls out of cache, every in-flight request for it misses at
once and they all hit Postgres — a cache stampede. `golang.org/x/sync/singleflight`
collapses them into a single read.

The subtlety is the context. The shared fetch must **not** inherit the context of
whichever caller happened to win the race, because if that client disconnects,
its cancellation propagates to everyone waiting behind it. The fetch runs on
`context.WithoutCancel(ctx)` with its own timeout. Both behaviours are tested
(`TestResolve_CoalescesConcurrentMisses`, `TestResolve_SurvivesCallerCancellation`).

### A cache outage degrades latency, not availability

Every cache error is logged and falls through to Postgres. The rate limiter
fails **open** for the same reason: it is a protection mechanism, not a
correctness one, and failing closed would turn a Redis blip into a total outage.

### Clicks are written asynchronously, in batches, and are droppable

Writing a row per redirect puts a database write on the hot path. Instead
`internal/analytics` buffers clicks in a channel drained by a worker pool that
flushes on batch size or on a ticker.

`Record` is non-blocking: if the buffer is full the event is dropped and counted
in `shortlink_clicks_dropped_total`. Analytics is sacrificed so redirects stay
fast — that is a deliberate trade, and the metric makes it visible.

The queue channel is deliberately **never closed**. A redirect can be in flight
while shutdown starts, and a send on a closed channel panics. Shutdown closes a
separate `done` channel; workers then drain whatever is buffered and flush it,
so nothing already accepted is lost. `Close` respects a context, so a wedged
database surfaces as a shutdown error rather than a hang.

### Clicks are inserted with `unnest`, not `CopyFrom`

`CopyFrom` is the fast path for bulk inserts in pgx, and it is the wrong tool
here. Clicks are buffered for seconds, so a link can be deleted between the
redirect and the flush; the resulting foreign key violation would fail the
entire batch. The insert passes parallel arrays through `unnest` with an
`EXISTS` filter, which drops exactly the orphan rows and commits the rest.
There is an integration test that deletes a link out from under a batch.

### The rate limiter is a Lua token bucket

A read-modify-write from Go would need a `WATCH`/`MULTI` retry loop or a lock;
under contention both cost more than the limiter saves. The bucket is a Lua
script, so it is one round trip and cannot interleave — 200 concurrent callers
are granted exactly the burst size, never more (`TestLimiter_IsAtomicUnderConcurrency`).

A token bucket rather than a fixed window because a fixed window lets a client
spend its whole quota in the last millisecond of one window and again in the
first of the next: double the intended rate at the boundary.

Buckets are keyed by API key when the caller is authenticated and by IP
otherwise, so one noisy tenant cannot spend the quota of everyone behind the
same NAT.

### `X-Forwarded-For` is ignored by default

The header is client-controlled. Trusting it unconditionally lets anyone forge
an address and walk around the per-IP limit by rotating a value. It is honoured
only when `TrustProxyHeaders` is set, which is a deployment fact, not a default.

### Redirect targets cannot point at private networks

An open redirector into a VPC is a standard SSRF pivot, and a shortener is
exactly the service that gets abused for it. `domain.NormalizeTargetURL` rejects
non-`http(s)` schemes, loopback, private, link-local (including
`169.254.169.254`, the cloud metadata endpoint), carrier-grade NAT and
`.local` hostnames.

### Aliases cannot shadow the router

The redirect route is mounted at the root, so an alias of `api` or `metrics`
would shadow real endpoints. `domain.ValidateAlias` keeps a reserved list.

### 302, not 301

A permanent redirect is cached by browsers indefinitely: the link could never be
retargeted or deleted, and click statistics would stop after the first visit.
The response also carries `Cache-Control: no-store` and
`Referrer-Policy: no-referrer`, so the short code does not leak to the
destination.

### Someone else's link is 404, not 403

A 403 confirms that a code exists. Ownership failures are reported as not found.

### API keys are stored hashed, and verification is cached

Only the SHA-256 of a key is persisted, so a database dump yields nothing
usable. Verification is cached in memory for 30 seconds — including **failures**,
because without a negative cache an attacker spraying random keys gets one free
database lookup per guess. The cache is bounded, so spraying cannot grow it
without limit. The trade is explicit: a revoked key keeps working for up to 30
seconds.

### Visitor IPs are pseudonymised

Storing raw addresses would make the clicks table personal data under GDPR for
no analytical gain: counting unique visitors only needs a stable identifier.
Addresses are stored as a salted SHA-256 — salted because an unsalted digest of
an IPv4 address is inverted by hashing all four billion of them. The service
refuses to start without `IP_HASH_SALT`.

### Metrics are labelled by route template, not path

Labelling by path would mint one Prometheus time series per short code and take
the metrics backend down. `chi`'s `RoutePattern()` gives `/{code}`.

### Liveness does not probe dependencies

`/healthz` reports only whether the process is alive. If it probed Postgres, a
database blip would fail every replica's liveness check at once and the
orchestrator would restart the whole fleet — turning a recoverable outage into
a cold start. `/readyz` does the probing, concurrently, and answers 503.

### Shutdown is ordered

`SIGTERM` cancels the root context. The HTTP server drains first, so no new
clicks are produced, and only then does the collector flush what is buffered.
The shutdown context is built with `context.WithoutCancel`, because the context
that was just cancelled cannot bound the work its own cancellation started.

---

## API

| Method | Path | Auth | Description |
|---|---|:--:|---|
| `POST` | `/api/v1/links` | ✔ | Shorten a URL, optionally with an alias and expiry |
| `GET` | `/api/v1/links` | ✔ | List your links, newest first, paginated |
| `GET` | `/api/v1/links/{code}` | ✔ | Fetch one of your links |
| `DELETE` | `/api/v1/links/{code}` | ✔ | Delete a link and its clicks |
| `GET` | `/api/v1/links/{code}/stats` | ✔ | Clicks, unique visitors, daily histogram, top referrers |
| `GET` | `/{code}` | — | Redirect, records the click |
| `GET` | `/healthz` | — | Liveness; stays public so an uptime monitor can reach it |
| `GET` | `/readyz` | — | Readiness; not exposed publicly in production |
| `GET` | `/metrics` | — | Prometheus metrics; not exposed publicly in production |
| `GET` | `/docs`, `/openapi.yaml` | — | API documentation |

Authentication is `Authorization: Bearer sl_...`. Every failure uses one
envelope, so clients parse errors without branching per endpoint:

```json
{ "error": { "code": "validation_error",
             "message": "must use the http or https scheme",
             "field": "target_url" } }
```

The full contract is in [`api/openapi.yaml`](api/openapi.yaml), embedded into
the binary so the published spec cannot drift from the build that serves it.

---

## Selling access through Telegram

A Telegram bot sells time-limited API access and doubles as a client for the
service. A user pays $10 in crypto through [@CryptoBot](https://t.me/CryptoBot),
gets five days, mints their own keys in the chat, and can shorten links and read
their click statistics without ever touching the HTTP API.

```
/buy                         → invoice in @CryptoBot → paid → "✅ access until 17 Sep"
/newkey                      → sl_...  (shown once; only its hash is stored)
https://go.dev/blog/context  → 🔗 https://sho.rt/gen1234
/links                       → your links, each with its click count
/stats gen1234               → clicks, unique visitors, top referrers, by day
```

Every key a user holds lives exactly as long as their paid access, and paying
again extends all of them at once. The bot is a separate process (`cmd/bot`)
that owns billing; the API only learned one new thing — a key can carry an
expiry.

### Shortening from the chat goes through the same service

The bot does not call its own HTTP API; it uses `service.LinkService` in
process. A link made by sending a URL to the chat passes exactly the checks one
made over HTTP does — the SSRF refusal, the reserved aliases, the collision
retry — because it is the same code, not a second implementation of the same
rules.

That does mean the bot bypasses the API's authentication middleware, so it has
to make those checks itself: the key exists, belongs to this user, is not
revoked, has not expired. Otherwise the chat would be a way to keep using the
service after the access ran out. There is an end-to-end test for exactly that.

### The bot never stores a secret

To shorten links on a user's behalf the bot needs to know *which* key to act
with, not what the key is. It issued those keys itself, so it records a
reference: `subscriptions.active_key_id`. A user's first key becomes the bot's
automatically — `/buy`, `/newkey`, send a URL — and `/usekey` switches it,
either by the number from `/keys` or by pasting the key, which is hashed on
arrival and matched against the stored hash.

Revoking the key the bot is using hands it the user's next newest key in the
same transaction, so it can never be left pointing at a key that no longer
works.

### Confirming a payment exactly once### Confirming a payment exactly once

Payment providers redeliver webhooks they did not see acknowledged, and a
webhook can also race the reconciler described below. Either way, one invoice can
arrive for confirmation several times at once.

Both paths call the same `ConfirmPayment`, whose guard is the `WHERE` clause of
its first statement:

```sql
UPDATE payments SET status = 'paid', paid_at = now()
WHERE invoice_id = $1 AND status <> 'paid'
RETURNING telegram_user_id, access_days
```

The second caller blocks on the row lock the first holds; once the first
commits it sees `paid` and updates nothing. No application lock, no window for
double-granting. The integration suite fires 20 concurrent confirmations at one
invoice and asserts exactly one succeeds and exactly five days are granted.

The buyer is notified only by the call that made the change, so a webhook
retried three times still sends one message.

### A forged webhook is the obvious attack

The webhook endpoint is public because Crypto Pay must reach it. Without
verification anyone could `POST {"status": "paid"}` and get access for free.
Every delivery is checked against an HMAC-SHA256 of the **raw** body — parsing
and re-encoding the JSON would change the bytes and fail genuine deliveries —
compared with `hmac.Equal`, in constant time, so the check does not leak how much
of a forged signature was right.

### Webhooks can be lost

The service might be down, the network might drop a delivery, or the webhook URL
might never have been configured. A reconciler asks the provider about recent
pending invoices every 20 seconds and settles what changed. It is also what
makes payments work in local development, where Crypto Pay has no public URL to
deliver to.

### The key limit holds when requests race

Two `/newkey` messages arriving together would both count the user's keys, both
see room, and both insert. `IssueKey` takes `SELECT ... FOR UPDATE` on the
user's subscription row — the one row every key issue and every renewal touches —
so the count and the insert behave as one step. Tested with 20 concurrent
requests against a limit of 3.

### Smaller decisions

- **Testnet by default, mainnet never by default.** Locally the bot defaults to
  testnet so a stray process cannot take money. The production overlay requires
  `CRYPTOPAY_NETWORK` explicitly, because defaulting the other way is also
  dangerous: a server left on testnet sells real access for coins that cost
  nothing.
- **Priced in USD, paid in crypto.** The invoice is fiat-denominated, so the coin
  moving between invoice and payment is the buyer's concern, not the seller's.
- **Private chats only.** `/newkey` in a group would post a secret to every
  member.
- **Renewal stacks.** `GREATEST(paid_until, now()) + 5 days`: paying early never
  forfeits time already bought, and paying after a lapse restarts from now.
- **Renewal revives lapsed keys**, so no integration needs a new secret — but
  never revoked ones.
- **Expired keys are rechecked after 5 seconds, not 30.** The auth cache holds a
  refusal for 30 seconds, which is right for unknown and revoked keys. An expired
  key comes back the moment its owner pays, and the moment after paying is
  exactly when they try it.
- **The bot token is redacted from errors.** It is part of every Bot API URL, and
  `net/http` prints the URL in transport errors, so the first network blip would
  have written full control of the bot into the logs.
- **The time zone database is embedded.** The runtime image is distroless and has
  none, so `time.LoadLocation` would fail in production and nowhere else.
- **Everything interpolated into a message is HTML-escaped** — key names, target
  URLs, and referrers, which arrive in a request header any visitor controls.
  Unescaped, a value like `<b>` would make Telegram reject the very message
  carrying the secret.
- **Click counts for a page of links are one query**, not one per link.

---

## Configuration

Everything comes from the environment; see [`.env.example`](.env.example) for
the full list with defaults. Configuration is validated at startup and **all**
problems are reported at once, so a misconfigured deployment does not fail one
variable per restart.

The only variable without a default is `IP_HASH_SALT`.

---

## Testing

```bash
make test              # unit tests only, no Docker needed
make test-integration  # adds testcontainers-backed integration tests
make cover             # HTML coverage report
```

77.9% of statements overall; 87–100% in the packages that carry logic.

**Unit tests** use hand-written fakes rather than generated mocks. The ports are
small and the tests need behaviour — a repository that really stores links, a
cache that can be told to fail — which generated mocks express as a pile of
expectation setup while adding a code-generation step to the build.

**Integration tests** run against real PostgreSQL 17 and Redis 7 in Docker via
testcontainers, because what they verify cannot fail in a fake: the `ON CONFLICT`
insert, the window-function pagination, the batched stats query, the `unnest`
write surviving a deleted link, and the Lua limiter's atomicity under 200
concurrent callers. The migrations themselves run as part of the setup. They are
skipped by `-short`, so the unit suite stays runnable without Docker.

The concurrency tests are the ones worth reading:
`TestResolve_CoalescesConcurrentMisses`, `TestResolve_SurvivesCallerCancellation`,
`TestCollector_CloseDrainsEverythingQueued`,
`TestCollector_RecordDuringShutdownDoesNotPanic`,
`TestLimiter_IsAtomicUnderConcurrency`. The whole suite runs under `-race` in CI.

CI (GitHub Actions) runs `gofmt`, `go vet`, `golangci-lint`, a tidiness check on
`go.mod`, the full race-enabled suite including integration tests, and a
container image build.

---

## Out of scope

Deliberate omissions, so their absence reads as a decision rather than an
oversight:

- **No user accounts or JWT.** API keys carry the same security lessons —
  hashing, revocation, bounded caching — without a login flow that adds
  nothing to the interesting parts.
- **No Kafka, no service mesh, no gRPC.** A single service with one database and
  one cache is the honest shape of this problem. Adding a broker to publish
  click events would be architecture as decoration.
- **No custom domains, QR codes, link editing, or a web UI.** More endpoints
  in the same shape, no new engineering.
- **No Grafana dashboards.** Metrics are exposed; wiring a dashboard is
  configuration, not code.

Real next steps, if this were a product: partition or roll up the `clicks` table
(the daily histogram scans raw rows and will not stay fast), move the redirect
behind a CDN, and add a background sweep for expired links.

---

## Layout

```
cmd/api               composition root: wiring, signals, graceful shutdown
cmd/shortlinkctl      operator CLI, shares the service packages
cmd/bot               Telegram bot, payment webhook and reconciler
internal/domain       entities, validation, sentinel errors — imports nothing local
internal/service      use cases, ports, cache-aside, singleflight
internal/analytics    click collector (worker pool) and IP pseudonymisation
internal/billing      invoices, idempotent confirmation, user-issued keys
internal/bot          Telegram commands and the buyer notifier
internal/cryptopay    Crypto Pay client and webhook signature check
internal/telegram     minimal Bot API client
internal/storage/     postgres (pgx) and rediscache (cache + Lua limiter)
internal/transport/   HTTP routing, middleware, JSON mapping
internal/shortid      random base62 code generation
internal/observability  slog setup and Prometheus metrics
migrations            embedded SQL, applied on start
api                   embedded OpenAPI spec
```

---

## Using this as a starting point

```bash
make rename MODULE_NEW=github.com/you/shortlink
```

## License

MIT — see [LICENSE](LICENSE).
