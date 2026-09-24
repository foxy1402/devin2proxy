# devin2proxy

An OpenAI-compatible HTTP gateway in front of the Devin CLI's own credential.

It takes the session token the `devin` CLI already stores on a machine and
speaks Devin's real backend protocol (Connect-RPC over protobuf), so anything
that talks to the OpenAI API — SDKs, Cline, Cursor, Continue, a shell script —
can talk to `swe-1.6` through it. No Cognition API key, no third-party
dependencies: a single static Go binary, or a ~25 MB container image.

Built with coding assistants in mind:

- `/v1/completions` does real fill-in-the-middle autocomplete (`suffix`), and
  `stop` sequences cancel the upstream generation instead of paying for tokens
  nobody will receive.
- A client that hangs up mid-stream cancels the backend request, so an
  abandoned autocomplete does not burn quota.
- Several Devin accounts can be pooled and rotated one per request — the
  free-tier limit is per account, so N accounts multiply throughput — with
  refusal-aware and quota-aware cooldowns.
- Outbound traffic can be rotated across SOCKS5/HTTP proxies the same way.
- A password-protected operator dashboard ships inside the same binary:
  account pool, proxy pool, model catalogue, live request log.

**Not affiliated with Cognition.** Every request spends the quota of your own
Devin account, and remains attributed to it on the wire. You are responsible
for how you use it.

The recovered wire protocol and how each fact about it was established live in
[docs/PROTOCOL.md](docs/PROTOCOL.md).

---

## Deploy on a VM with Portainer

The container is env-driven end to end; nothing needs to be baked into the
image. The published image is
[`ghcr.io/foxy1402/devin2proxy:latest`](https://github.com/foxy1402/devin2proxy/pkgs/container/devin2proxy)
— pulling needs no login, the repository is public.

You need:

1. A machine running Portainer (tested shape: a Google e2-micro — 1 vCPU,
   1 GB RAM is plenty; this is a small Go binary).
2. A Devin session token — see [Getting a token](#getting-a-token).

In Portainer: **Stacks → Add stack → Web editor**, paste the whole block
below, then in the same screen's **Environment variables** section add the
four values it references (`DEVIN2PROXY_API_KEY`, `DEVIN_TOKEN`,
`DEVIN2PROXY_DASHBOARD_PASSWORD`, and optionally `DEVIN2PROXY_TLS_NAMES` with
the machine's public IP). Deploy, and you have an HTTPS gateway.

```yaml
# devin2proxy — Portainer stack for a public VM.
services:
  devin2proxy:
    image: ghcr.io/foxy1402/devin2proxy:latest
    container_name: devin2proxy
    restart: unless-stopped
    ports:
      # Published on every interface: the cloud firewall decides who can
      # actually reach it. The API key and the TLS certificate are the gates.
      - "8788:8788"
    environment:
      DEVIN2PROXY_ADDR: 0.0.0.0:8788
      DEVIN2PROXY_API_KEY: ${DEVIN2PROXY_API_KEY}
      DEVIN_TOKEN: ${DEVIN_TOKEN}
      # Self-signed certificate generated into /data on first start and reused
      # from then on. Its SHA-256 fingerprint is printed at startup — check it
      # once, then hand clients /data/tls.crt to trust.
      DEVIN2PROXY_TLS: "1"
      DEVIN2PROXY_TLS_NAMES: ${DEVIN2PROXY_TLS_NAMES:-}
      # The dashboard: password-gated, every button behind that gate. With
      # TLS on, the session cookie is marked Secure and the password never
      # crosses the wire in cleartext.
      DEVIN2PROXY_DASHBOARD: "1"
      DEVIN2PROXY_DASHBOARD_PASSWORD: ${DEVIN2PROXY_DASHBOARD_PASSWORD}
      DEVIN2PROXY_DASHBOARD_ALLOW_REMOTE: "true"
      # Optional: a pool of accounts to rotate instead of the single DEVIN_TOKEN.
      # DEVIN2PROXY_TOKENS: ${DEVIN2PROXY_TOKENS:-}
    volumes:
      # /data holds dashboard.json (password hash, session secret, managed
      # accounts and routes) and tls.crt / tls.key. The volume is what makes
      # the certificate stable across restarts — TOFU depends on that.
      - devin2proxy-data:/data
    # Everything is supplied by env, so the process writes nothing outside
    # /data and the root filesystem can be read-only.
    read_only: true

volumes:
  devin2proxy-data:
```

First start prints the certificate fingerprint:

```
tls: generated a self-signed certificate at /data/tls.crt
tls: SHA-256 fingerprint AB:CD:…
listening on https://0.0.0.0:8788
```

Verify from anywhere:

```bash
curl -sk https://<your-ip>:8788/healthz
# {"credential":"loaded","status":"ok"}

curl -sk https://<your-ip>:8788/v1/chat/completions \
  -H "Authorization: Bearer $DEVIN2PROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"swe","messages":[{"role":"user","content":"say pong"}]}'
```

The same file lives in this repo as [`compose.yaml`](compose.yaml), so
`docker compose up -d` works anywhere compose is available.

### Getting a token

The credential is a `devin-session-token$…` value the Devin CLI mints when you
sign in. Install the CLI from Devin's site, log in once, and copy the
`windsurf_api_key` line:

- Windows: `%APPDATA%\devin\credentials.toml`
- Linux/macOS: `$XDG_CONFIG_HOME/devin/credentials.toml`, falling back to
  `~/.local/share/devin/credentials.toml`

The token's JWT carries **no `exp` claim** — it does not expire — so this is a
one-time step per account. Once the gateway is running you can also add more
accounts from the dashboard's **Add account (OAuth)** button, which performs
the CLI's PKCE sign-in itself and never needs the CLI on the server.

### Plain `docker run`

```bash
docker run -d --name devin2proxy \
  -p 8788:8788 \
  -e DEVIN2PROXY_API_KEY='sk-devin-pick-something-long' \
  -e DEVIN_TOKEN='devin-session-token$…' \
  -e DEVIN2PROXY_TLS=1 \
  -v devin2proxy-data:/data \
  --read-only \
  ghcr.io/foxy1402/devin2proxy:latest
```

To build the image yourself instead of pulling it: `docker build -t
devin2proxy .` — the Dockerfile is a multi-stage build (static
`CGO_ENABLED=0` binary, nonroot uid 10005, alpine runtime, healthcheck on
`/healthz`).

## Configuration

Every setting is an environment variable; a `config.json` beside the binary is
the desktop alternative (looked for in the working directory first, then
beside the executable; `-config <path>` names one explicitly). On a desktop
first run it is written with a freshly generated API key, mode `0600`. In a
container, with `DEVIN2PROXY_API_KEY` set, **nothing is ever written to the
config file** — which is what makes `read_only: true` work.

| Variable | `config.json` field | Default | Meaning |
|---|---|---|---|
| `DEVIN2PROXY_ADDR` | `addr` | `127.0.0.1:8788` (image: `0.0.0.0:8788`) | listen address |
| `DEVIN2PROXY_API_KEY` | `api_key` | generated | the one key gating `/v1`; required — with no key the server fails closed with `503` |
| `DEVIN_TOKEN` / `WINDSURF_API_KEY` | — | — | session token, overriding the CLI's file |
| `DEVIN_CREDENTIALS_PATH` | — | the CLI's path | read a mounted `credentials.toml` instead |
| `DEVIN2PROXY_TOKENS` | `tokens` | — | account pool, comma-separated; rotates one account per request |
| `DEVIN2PROXY_TOKENS_FILE` | `tokens_file` | — | same, one token per line (`#` comments ok) |
| `DEVIN2PROXY_PROXIES` | `proxies` | — | outbound routes: `socks5://`, `socks5h://`, `http://`, `https://`, or `direct`; rotates one per request |
| `DEVIN2PROXY_PROXIES_FILE` | `proxies_file` | — | same, one URL per line |
| `DEVIN2PROXY_QUOTA_COOLDOWN` | `quota_cooldown` | `1` | a rate-limited account is held until its quota resets instead of retried every 30 s |
| `DEVIN2PROXY_TLS` | `tls` | off | serve HTTPS; generates a self-signed cert beside the config when none is named |
| `DEVIN2PROXY_TLS_CERT_FILE` / `_KEY_FILE` | `tls_cert_file` / `tls_key_file` | `tls.crt` / `tls.key` beside the config | a real certificate, once you have a domain |
| `DEVIN2PROXY_TLS_NAMES` | `tls_names` | — | extra SANs for the generated cert — **put the machine's public IP here**; `127.0.0.1`, `::1`, `localhost` are always included |
| `DEVIN2PROXY_DASHBOARD` | `dashboard` | `1` | serve `/dashboard/` |
| `DEVIN2PROXY_DASHBOARD_PASSWORD` | `dashboard_password` | generated, printed once | dashboard login |
| `DEVIN2PROXY_DASHBOARD_ALLOW_REMOTE` | `dashboard_allow_remote` | `false` | allow the dashboard from outside the machine |
| `DEVIN2PROXY_MODEL` | `model` | `swe-1-6-slow` | default model; a non-backend uid is a start-up error |
| `DEVIN2PROXY_MODELS` | `models` | `swe-1-6-slow, swe-1.6, swe` | advertised ids |
| `DEVIN2PROXY_MIN_MAX_TOKENS` | `min_max_tokens` | `8192` | floor on a client's `max_tokens` — see [Models](#models) |
| `DEVIN2PROXY_MAX_CONCURRENT` | `max_concurrent` | `2` | in-flight backend streams |
| `DEVIN2PROXY_ALLOW_ORIGINS` | `allow_origins` | `*` | CORS |
| — | `request_timeout_seconds` | `600` | per-request ceiling (config file only) |

The credential file (when used) is re-read **per request**, so re-running
`devin auth login` takes effect without restarting the proxy.

## Security model

Two independent surfaces, deliberately:

- **`/v1` — one fixed API key.** Every route requires `Authorization: Bearer
  <key>` (or `x-api-key`). No key configured → every request is refused with
  `503`; the server never serves the account unprotected. A wrong key gets
  `401` after a 250 ms floor, which makes online guessing of the key
  pointless. Correct keys never pay the delay.
- **`/dashboard/` — a password, and nothing else.** Every page, button and
  data endpoint behind the login; an unauthenticated visitor gets the sign-in
  form and nothing more. Hashes are PBKDF2-HMAC-SHA256 (210k iterations);
  three failed logins ban the client address, doubling from 1 minute to a
  24-hour cap, persisted across restarts. The password is never stored or
  logged.

For a bare-IP deployment there is no CA that will issue a certificate, so the
gateway ships its own: `DEVIN2PROXY_TLS=1` generates an ECDSA P-256 self-signed
pair on first start (825-day validity, the browser ceiling) and prints its
SHA-256 fingerprint. Because `/data` is a volume, the fingerprint is **stable
across restarts and redeploys** — trust-on-first-use that actually works.
Clients trust the `tls.crt` file (curl `-k` for the first contact, then
`--cacert`; Node `NODE_EXTRA_CA_CERTS`; browsers just need the file imported
once). The dashboard session cookie is `HttpOnly`, `SameSite=Strict`, scoped to
`/dashboard`, and gets the `Secure` flag automatically when served over TLS —
which is why TLS is the recommended remote setup, not a reverse proxy in front
of plain HTTP.

Binding to a non-loopback address logs a warning at startup; the cloud
firewall is what actually decides who reaches the port.

## Endpoints

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/v1/chat/completions` | streaming (SSE) and non-streaming; tools, multi-turn, `stop` |
| `POST` | `/v1/completions` | streaming and non-streaming; **fill-in-the-middle via `suffix`**, `stop`, `echo` |
| `GET`  | `/v1/models` | advertised ids |
| `POST` | `/v1/embeddings` | `501` — the backend has no embeddings capability |
| `GET`  | `/healthz` | no auth; `status`, credential/pool state, `accounts_available`, `proxies_available` |
| `GET`  | `/dashboard/` | operator panel, under its own password |

## Pointing clients at it

Any OpenAI-compatible client works by setting the base URL and key.

**Cline / Cursor / Continue / Roo** — choose "OpenAI Compatible", then:

| setting | value |
|---------|-------|
| Base URL | `https://<your-ip>:8788/v1` (or `http://127.0.0.1:8788/v1` locally) |
| API key | your `DEVIN2PROXY_API_KEY` |
| Model | `swe-1-6-slow` (or the alias `swe`) |

With the self-signed certificate, distribute `tls.crt` to machines that use
the gateway (import into the OS trust store, or point the client at it —
Cursor/VS Code: `--cert` / `NODE_EXTRA_CA_CERTS`). Disable any
embedding/indexing feature: `/v1/embeddings` returns `501` by design.

**OpenAI Python SDK**

```python
import httpx
from openai import OpenAI

client = OpenAI(base_url="https://<your-ip>:8788/v1",
                api_key="sk-devin-…",
                http_client=httpx.Client(verify="/path/to/tls.crt"))
reply = client.chat.completions.create(
    model="swe", messages=[{"role": "user", "content": "hello"}])
```

**OpenAI Node SDK**

```js
import OpenAI from "openai";
const client = new OpenAI({ baseURL: "https://<your-ip>:8788/v1", apiKey: "sk-devin-…" });
```

## Models

| id | behaviour |
|----|-----------|
| `swe-1-6-slow` | the default, and the only uid a free-tier account serves |
| `swe-1.6`, `swe` | aliases resolving to it |
| `swe-1-6-fast` | a real backend uid; the proxy forwards it, the backend refuses it below a paid plan |

An unrecognised model name falls back to the default and logs the
substitution, so a client hardcoded to `gpt-4` simply works. The backend
declares a 200k context window and 128k max output for `swe-1-6-slow`.

**`min_max_tokens` (default 8192) exists because the backend bills the model's
reasoning against the same budget as the answer.** Autocomplete clients
routinely ask for 64–256 tokens; without the floor the reasoning exhausts the
budget and the answer comes back empty with `finish_reason: "length"`.
Measured on a small FIM request: 130 prompt tokens, 663 completion tokens for
a three-token answer. `max_tokens` is a ceiling, not a spend — raising the
floor is close to free.

Other measured facts: **no usable vision** (images are transmitted with
verified field numbers but the model identifies colour at chance level), and
thinking is unavoidable — `reasoning_content` streams before `content` on
every request, and clients that don't know the field ignore it.

### Client parameters actually honoured

| parameter | behaviour |
|---|---|
| `max_tokens` / `max_completion_tokens` | honoured as a ceiling, raised to the `min_max_tokens` floor, not capped at the top |
| `temperature` | honoured, clamped to `(0, 2]` — `0` is what IDEs send and the backend refuses it, so it becomes `0.001` |
| `top_p` | honoured, clamped to `(0, 1]` |
| `stop` | honoured, and cancels the upstream generation |
| `stream`, `stream_options.include_usage` | honoured |
| `tools`, `tool_choice: auto/none` | honoured; `required`/named-function → `400` (the backend has no field for it) |
| `n`, `seed`, `logprobs`, `response_format`, penalties, `reasoning_effort` | ignored silently; unknown JSON fields are dropped, not rejected |

The context window is not a request parameter in OpenAI's API at all — set it
in your IDE to at most 200k.

## Multiple accounts

The account limit is enforced per account, so a pool of free-tier accounts
rotates one account per request and multiplies throughput. Rotation is
round-robin and stateless — every request opens a fresh backend trajectory and
the client resends history each turn, so which account answers is irrelevant
to the result.

- A refused account steps out for 5 minutes; a rate-limited one for 30
  seconds — except with quota cooldown on (the default), where a 429 triggers
  one background status call and an exhausted account is held until its quota
  actually resets (capped at 12 hours). Quota evidence can only ever *extend*
  a cooldown the backend earned, never create one.
- A `429` is answered by the next account immediately, not by retrying the
  spent one.
- Client cancels and transport failures never cool anything — an IDE
  interrupts constantly, and that is not the account's fault.
- A refusal that empties the whole pool is treated as provider-side, holding
  accounts for 30 s rather than 5 minutes each.

Feed the pool via `DEVIN2PROXY_TOKENS(_FILE)` (shown read-only in the
dashboard), or manage it live from the dashboard: **Add account (OAuth)**
runs the CLI's PKCE sign-in in-process — you sign in on a Devin page, paste
the code back, the token is verified against the backend before it joins the
pool. The proxy only ever *reads* `credentials.toml`; it never writes to your
Devin configuration.

## Outbound proxies

Routes rotate one per request, independently of the account pool:

```
socks5://127.0.0.1:1080
socks5h://user:pass@proxy.example:1081
http://user:pass@proxy.example:3128
direct
```

`socks5h` resolves the hostname at the proxy. `http(s)` uses `CONNECT`.
`direct` includes the machine's own connection. Credentials never appear in
logs — a route is labelled `scheme://host:port`. A route is only cooled (5
minutes) when confirmed unusable: undialable, handshake failed, or it refused
our credentials. A backend `429` through a working route leaves the route
alone — that is the account's problem, not the exit's.

## The dashboard

`/dashboard/` is a single-page operator panel with four tabs, all behind the
password:

- **Accounts** — identity, plan and quota counters per account (the same
  `week 41% remaining` figures the Devin CLI prints), rotation state,
  OAuth/paste/CLI account intake, and a **test** button that sends one real
  short completion through one account and lifts its cooldown on success.
  Probes are throttled to one per account per 30 s — they spend quota.
- **Models** — what this proxy serves next to what the backend's own
  catalogue says: context window, limits, price rows, plan gates. Read
  through a pooled account, cached ten minutes.
- **Proxies** — the route list; **Test each route** opens a real tunnel and
  reports the exit address. Passwords are shown masked and the mask round-trips
  (saving a masked list keeps the real credentials).
- **Logs** — live SSE view of every request and the gateway's own log lines:
  upstream status, account, route, duration, token counts, and the backend's
  own error text — which matters because Devin reports refusals as HTTP 200
  with the error inside the stream.

State lives in `dashboard.json` beside the config, mode `0600`: password
hash, session secret, managed accounts and routes, the failed-login record.
It holds account tokens in cleartext — treat it like a credential file.
Precedence for both pools is environment → `config.json` → dashboard's own
list; a list supplied by config is shown but not editable from the browser.

## Development

Requires Go 1.27+; the module has **zero third-party dependencies** (the
protobuf codec, SOCKS5 and HTTP CONNECT are hand-rolled).

```bash
go build ./... && go vet ./... && go test ./...
```

The test suite never touches the network: everything runs against in-process
stubs (`httptest`), including the Connect framing, the cooldown rules and the
dashboard's auth ladder. Live checks against the real backend are
env-gated (`DEVIN2PROXY_TEST_ACCOUNT=1`, `DEVIN2PROXY_TEST_PROXIES=…`) so a
stray `go test` cannot spend quota.

`tools/` holds the black-box suites and probes:

```bash
cd tools && npm install openai
set DEVIN2PROXY_BASE=http://127.0.0.1:8788/v1   # or https://… with the cert
set DEVIN2PROXY_API_KEY=sk-devin-…
node ide-sim-test.mjs      # 14 checks: FIM, stop, images, tool round trip, abort, 501
node openai-sdk-test.mjs   # 10 checks: models, chat, usage, streaming, tools, 401
```

Plus offline stand-ins that cost no quota: `rotation-probe.mjs` (records
which credential each request arrived on), `tunnel-probe.mjs` (a local
SOCKS5/CONNECT proxy), `quota-probe.mjs` with `zero-quota-fixture.mjs`
(exercises the quota-hold path against a recorded status response).

The container story is verified the same way: both suites pass end-to-end
through the published image over HTTPS with the generated certificate
actually verified by the client, the certificate survives a full container
destroy-and-recreate with the same fingerprint, and the root filesystem is
read-only throughout.

## Repository layout

```
cmd/devin2proxy/     the server: config, env, TLS, startup
cmd/devin-call/      one-shot live request — the field-bisect workhorse
cmd/devin-status/    who a token belongs to: email, plan, quota resets
cmd/devin-models/    the backend's model catalogue (-live for the real call)
cmd/devin-probe/     the local capture relay used to reverse-engineer the protocol
cmd/devin-decode/    replays a captured request through the typed decoder
internal/pb/         hand-rolled protobuf reader/writer, no dependencies
internal/devin/      wire schema, Connect framing, credentials, HTTP client,
                     the account pool and the rotating egress pool
internal/openai/     OpenAI types, request/response translation, FIM and stop handling
internal/server/     HTTP routing, API-key gate, SSE, per-request events
internal/dashboard/  the operator panel: auth, accounts, proxies, models, logs
internal/eventlog/   bounded event history + live subscribers + log tap
tools/               test suites and probes (see Development)
docs/PROTOCOL.md     the recovered wire format, and how each fact was verified
Dockerfile           multi-stage build → ghcr.io/foxy1402/devin2proxy
compose.yaml         the Portainer stack above, as a file
.github/workflows/   push to main → build → push :latest and :sha-<commit> to GHCR
```

## Limitations

- No embeddings (`501` by design) — turn off codebase indexing in clients.
- Thinking dominates latency: every request pays for reasoning first.
- No usable vision — see [Models](#models).
- Fill-in-the-middle is emulated with an instruction prompt, not native;
  `stop` sequences are the main defence.
- A dead mid-stream connection cannot be resumed; the client must resend.
- Uses your real account and consumes its quota. A proxy changes the exit
  address and nothing else — requests still carry your token.

## Troubleshooting

- **Empty autocomplete with `finish_reason: "length"`** — the budget went to
  reasoning; raise `min_max_tokens`.
- **`502 credential_rejected`** — no pool: run `devin auth login`, no restart
  needed. With a pool: this only surfaces when *every* account was refused;
  check `/healthz`.
- **`502 model_unavailable`** — the backend's `failed_precondition`; on a
  free-tier account only `swe-1-6-slow` is served.
- **`503 no API key`** — set `DEVIN2PROXY_API_KEY` or delete `config.json`
  to regenerate.
- **`502 egress_unavailable`** — the account is fine, the exit is not; the
  message names the route and the failure (`dial`, `socks5 auth`, `CONNECT`).
- **`400 invalid_argument`** — a malformed backend request, almost always a
  zero-valued sampling field; the proxy guards against this, so if it comes
  back it is a bug here.
- **Client reports a certificate name mismatch** — the public IP was not in
  `DEVIN2PROXY_TLS_NAMES` when the cert was generated. Delete `tls.crt` /
  `tls.key` from the volume, add the IP, restart; note the fingerprint
  changes.
