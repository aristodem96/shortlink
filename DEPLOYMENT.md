# Deployment

Deploying shortlink to a single Ubuntu server with HTTPS.

Everything runs in Docker: the service, PostgreSQL, Redis, and Caddy as the
TLS-terminating reverse proxy. The only things you install on the host are
Docker itself and a firewall rule.

**Before you start you need:** a server with a public IP, a domain you control,
and SSH access to the server as a non-root user with `sudo`.

---

## Where the project lives

Put the checkout at:

```
/opt/shortlink
```

`/opt` is the conventional location for self-contained application software that
is not managed by the system package manager, which is exactly what this is.
`~/shortlink` works too and is simpler if the server has one user; avoid `/root`,
because the whole point of the setup below is that Docker runs as a normal user.

Two things must sit together in that directory, because compose resolves
`./Caddyfile` and `build: context: .` relative to the compose file:

```
/opt/shortlink/
├── docker-compose.yml          from git
├── docker-compose.prod.yml     from git
├── Caddyfile                   from git
├── .env                        YOU create this, never committed
└── ... the rest of the repo
```

**Your data does not live here.** PostgreSQL stores its files in a Docker volume
under `/var/lib/docker/volumes/`, not in the project folder. Deleting or moving
the checkout does not delete the database — and conversely, backing up
`/opt/shortlink` does not back up your data. See [Backups](#backups).

The project name is pinned to `shortlink` in `docker-compose.yml`, so volumes are
named `shortlink_pgdata` regardless of the directory. Without that pin, compose
derives the name from the folder, and renaming the folder later would silently
create a fresh empty database.

---

## Steps

### 1. Point the domain at the server

Create an A record with your registrar:

```
Type   Name     Value
A      links    203.0.113.45        your server's public IP
```

Verify it resolves before going further — Let's Encrypt will not issue a
certificate otherwise:

```bash
dig +short links.example.com
```

### 2. Install Docker

```bash
ssh user@203.0.113.45
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker $USER
exit
```

Log out and back in so the group membership applies, then confirm Docker works
without `sudo`:

```bash
ssh user@203.0.113.45
docker version
```

### 3. Open the firewall

```bash
sudo ufw allow OpenSSH
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw enable
```

Port 443 serves HTTPS; port 80 is what Let's Encrypt uses to verify the domain.
Nothing else needs to be reachable: the production overlay publishes no port for
PostgreSQL, Redis, or the API itself.

### 4. Clone the repository

```bash
sudo mkdir -p /opt/shortlink
sudo chown $USER:$USER /opt/shortlink
git clone https://github.com/aristodem96/shortlink.git /opt/shortlink
cd /opt/shortlink
```

### 5. Generate the secrets

```bash
openssl rand -base64 32     # POSTGRES_PASSWORD
openssl rand -base64 32     # REDIS_PASSWORD
openssl rand -base64 32     # IP_HASH_SALT
```

### 6. Write the .env file

```bash
cp .env.prod.example .env
nano .env
chmod 600 .env
```

```bash
SHORTLINK_DOMAIN=links.example.com     # bare hostname, no https://
POSTGRES_USER=shortlink
POSTGRES_PASSWORD=<first generated value>
POSTGRES_DB=shortlink
REDIS_PASSWORD=<second>
IP_HASH_SALT=<third>
LOG_LEVEL=info
```

Do not change `IP_HASH_SALT` after the first start: rotating it invalidates every
stored visitor hash, so unique-visitor counts restart from zero.

### 7. Start the stack

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build
```

The first build takes a few minutes. If a secret is missing, compose refuses to
start and names the variable.

### 8. Check that it came up

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml ps
```

All four services — `postgres`, `redis`, `api`, `caddy` — must show `running`.
A service stuck in `created` did not start; read its logs.

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml logs caddy \
  | grep -i certificate
```

Wait for `certificate obtained successfully`. This can take up to a minute.

### 9. Issue an API key

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml \
  exec api /shortlinkctl issue-key -name production
```

The secret is printed once and only its hash is stored, so save it now.

`make key` does **not** work here: it runs `go run` on the host and connects
through `localhost:5432`, a port the production overlay deliberately does not
publish.

### 10. Verify from outside

Run this from your own machine, not from the server:

```bash
KEY=sl_...

curl -X POST https://links.example.com/api/v1/links \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"target_url":"https://go.dev","alias":"test"}'

curl -i https://links.example.com/test
```

Expect `302` with `Location: https://go.dev`. Confirm the proxy exposes what it should and hides what it should not:

```bash
curl -s -o /dev/null -w 'healthz %{http_code}\n' https://links.example.com/healthz   # 200
curl -s -o /dev/null -w 'metrics %{http_code}\n' https://links.example.com/metrics   # 404
curl -s -o /dev/null -w 'readyz  %{http_code}\n' https://links.example.com/readyz    # 404
```

`/healthz` is public on purpose so an external uptime monitor can reach it; it
answers `{"status":"ok"}` without touching a dependency. `/metrics` is a
process-wide dump with no per-tenant breakdown, and `/readyz` names the
dependency that is down, so both stay behind the proxy — reach them over the
compose network or an SSH tunnel.

---

## Day-to-day

A shell alias saves typing both compose files every time. Add to `~/.bashrc`:

```bash
alias dcp='docker compose -f docker-compose.yml -f docker-compose.prod.yml'
```

| Task | Command |
|---|---|
| Deploy a new version | `cd /opt/shortlink && git pull && dcp up -d --build` |
| Follow the logs | `dcp logs -f api` |
| Service status | `dcp ps` |
| Issue a key | `dcp exec api /shortlinkctl issue-key -name <label>` |
| Open a database shell | `dcp exec postgres psql -U shortlink shortlink` |
| Restart one service | `dcp restart api` |
| Stop everything | `dcp down` |

Migrations run automatically on start, so an upgrade is just `up -d --build`.
The graceful shutdown flushes buffered clicks before the old container exits.

> **Never run `dcp down -v` on the server.** The `-v` flag deletes the volumes,
> and with them the entire database.

---

## The Telegram bot

The bot sells API access and is part of the production stack. It needs two
tokens and one setting in the Crypto Pay app.

### 1. Create the bot

Talk to [@BotFather](https://t.me/BotFather), send `/newbot`, and copy the token
it gives you into `.env` as `TELEGRAM_BOT_TOKEN`.

### 2. Create a Crypto Pay app

**Start on testnet.** Open [@CryptoTestnetBot](https://t.me/CryptoTestnetBot),
go to *Crypto Pay → Create App*, and copy the token into `CRYPTOPAY_TOKEN`. Set
`CRYPTOPAY_NETWORK=testnet`. Testnet is a full parallel system with coins that
cost nothing, so the whole purchase can be walked through end to end.

A testnet token only works against testnet, and a mainnet token only against
mainnet — a mismatch fails every invoice with `UNAUTHORIZED`.

### 3. Point the app's webhook at the server

In the app's settings, under *Webhooks*, set:

```
https://links.example.com/webhooks/cryptopay
```

Caddy routes that path to the bot. Every delivery is verified against its HMAC
signature, so the endpoint being public is safe.

The webhook is the fast path. If it is missing or a delivery is lost, the bot's
reconciler still settles the payment within about 20 seconds — so a payment that
confirms slowly means the webhook is not configured, not that it failed.

### 4. Start and try it

```bash
dcp up -d --build
dcp logs bot | grep 'starting bot'     # confirms the network it is using
```

In Telegram: `/start`, `/buy`, pay in @CryptoTestnetBot, wait for "✅", then
`/newkey` and call the API with the key.

### 5. Go live

Create a second app in [@CryptoBot](https://t.me/CryptoBot) for mainnet, set
its webhook the same way, then in `.env`:

```bash
CRYPTOPAY_TOKEN=<the mainnet app token>
CRYPTOPAY_NETWORK=mainnet
```

and `dcp up -d`. The log line from step 4 must now say `mainnet`.

Before taking real money, check the legal side where you operate: selling a
service is commercial activity, and in Ukraine that may mean registering as a
sole proprietor (ФОП) for tax.

---

## Backups

The database lives in the `shortlink_pgdata` Docker volume, not in
`/opt/shortlink`. Copying the project directory does not back up your data.

```bash
cd /opt/shortlink
dcp exec -T postgres pg_dump -U shortlink shortlink | gzip > ~/backup-$(date +%F).sql.gz
```

A daily cron entry:

```cron
0 3 * * * cd /opt/shortlink && docker compose -f docker-compose.yml -f docker-compose.prod.yml exec -T postgres pg_dump -U shortlink shortlink | gzip > ~/backups/shortlink-$(date +\%F).sql.gz
```

Restore into a running stack:

```bash
gunzip -c backup-2026-09-07.sql.gz | dcp exec -T postgres psql -U shortlink shortlink
```

Copy the dumps off the server. A backup that lives only on the machine it
protects is not a backup.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Caddy never obtains a certificate | DNS does not point at the server yet, or port 80 is closed | check `dig +short <domain>` and `sudo ufw status` |
| compose refuses to start | a secret is empty in `.env` | it names the variable; fill it in |
| Short links come back with `https://https://…` | `SHORTLINK_DOMAIN` includes the scheme | use the bare hostname |
| Short links come back with `localhost` | `.env` missing or `SHORTLINK_DOMAIN` unset | check the file is in `/opt/shortlink` |
| `api` stays in `created` | a published port is taken on the host | `dcp logs api`; nothing but Caddy should publish ports |
| Every click shows the same visitor | `TRUST_PROXY_HEADERS` is not set | the overlay sets it to `true`; confirm you passed both `-f` files |
| `permission denied` from Docker | the group change has not applied | log out and back in after `usermod -aG docker` |
| `password authentication failed for user "shortlink"` | the Postgres volume already exists from an earlier run with a different password | see below |

Configuration is validated at startup and reports every problem at once, so a
misconfigured deployment does not fail one variable per restart.

### The password that will not take

PostgreSQL reads `POSTGRES_PASSWORD` **only when it initialises an empty data
directory**. If a volume already exists — because the base compose file was
started once to "try it out", or because a password was changed after the first
run — the old credentials stay in effect and the api crash-loops with:

```
fatal: postgres: apply migrations: failed to connect to `user=shortlink database=shortlink`:
       failed SASL auth: FATAL: password authentication failed for user "shortlink" (SQLSTATE 28P01)
```

The error names authentication, never the volume, which is what makes it
confusing. On a server that has no data worth keeping:

```bash
dcp down -v          # deletes the volume, and with it the database
dcp up -d --build
```

If the database *does* hold data, change the password inside Postgres instead of
deleting anything:

```bash
dcp exec postgres psql -U shortlink -c "ALTER USER shortlink PASSWORD 'the new value';"
```

then put the same value in `.env` and `dcp up -d` to restart the api against it.

The lesson for a first deployment: start with the production overlay from the
beginning. Running the plain `docker compose up` once to test creates a volume
with the development password baked in.

---

## Running behind an existing nginx

If the server already terminates TLS with nginx, drop the `caddy` service and
publish the API on the loopback only:

```yaml
  api:
    ports: !override ["127.0.0.1:8080:8080"]
```

Then proxy to `127.0.0.1:8080`, keep `TRUST_PROXY_HEADERS=true`, and make sure
nginx sets `X-Forwarded-For` — without it every click is attributed to the proxy
and the per-IP rate limit treats the whole internet as one client.
