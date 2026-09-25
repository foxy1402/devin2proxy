# The Devin CLI wire protocol, as recovered

This documents what the `devin` CLI actually sends when it chats, and how each
fact was established. Everything marked **verified** was observed on the wire or
confirmed by a live request; anything inferred is labelled as such.

## Where it talks

The CLI does not use an OpenAI-style `/v1/chat/completions` JSON API. It speaks
**Connect-RPC over HTTP/1.1 with protobuf payloads**:

```
POST https://server.codeium.com/exa.api_server_pb.ApiServerService/GetChatMessage
Content-Type: application/connect+proto
Connect-Protocol-Version: 1
Authorization: Basic <token>-<token>
```

**Verified.** The base host comes from `api_server_url` in the CLI's
credentials file, and can be redirected with the `WINDSURF_API_SERVER_URL`
environment variable.

## How it was captured

No TLS interception was needed, which matters: it needs no CA certificate
install and no administrator rights.

```bat
REM terminal 1 — a local relay that logs everything and forwards it upstream
go run ./cmd/devin-probe -addr 127.0.0.1:8787 -relay https://server.codeium.com -out captures-relay

REM terminal 2 — the real CLI, pointed at the relay
set WINDSURF_API_SERVER_URL=http://127.0.0.1:8787
devin -p "Reply with exactly the word: pong" --respect-workspace-trust false
```

The CLI honours the override and speaks plain HTTP to it, so the full request
and response — headers, body, and every streamed frame — land in
`captures-relay/` as hex plus a decoded summary.

A capture-only probe (no `-relay`) is not enough: the CLI runs preflight calls
(`GetUserStatus`, `GetCliModelConfigs`, `GetCliTeamSettings`) before it will
start a chat, and it aborts with `failed to start ACP agent session` if they do
not succeed. Relaying keeps the CLI happy so it proceeds to `GetChatMessage`.

> **The capture files contain your session token in cleartext**, in the
> `Authorization` header and inside the request body. They are gitignored. Treat
> them as secrets and delete them when done.

## Credential

`%APPDATA%\devin\credentials.toml` holds:

```toml
windsurf_api_key = "devin-session-token$<HS256 JWT>"
api_server_url = "https://server.codeium.com"
devin_webapp_host = "..."
devin_api_url = "https://api.devin.ai"
```

The JWT payload is only `{"session_id": "windsurf-session-…"}` with **no `exp`
claim**, so the token does not expire on its own; re-login through the CLI
replaces it.

### The auth header is not real Basic auth

```
Authorization: Basic devin-session-token$eyJ…-devin-session-token$eyJ…
```

**Verified.** That is the literal token, a hyphen, and the token again — 385
bytes for a 189-byte token. It is *not* base64 of `user:password`, and decoding
it as such fails. The client reproduces this exactly.

### The `devin-session-token$` prefix selects the validator

**Verified.** The prefix is not decoration; it is what routes the credential to
the Devin-token validator instead of the API-key validator. Sending a
well-formed token with a tampered signature still reaches the Devin validator
and comes back from *it*:

```
failed to get primary API key; try logging out and logging in again:
failed to validate Devin token: Invalid token
```

whereas a value without the prefix is refused earlier and differently:

```
unauthenticated: invalid api key
```

The second message is what the CLI's own website-issued API key produces, which
is the useful part: **an `sk-devin-…` key from the Devin dashboard cannot be used
as an upstream credential**, and neither can this proxy's client key, which has
the same shape. The `sk-devin-` prefix belongs to the proxy's own bearer-token
check and to Devin's API-key endpoint; the chat backend wants a session token.

This is what makes multi-account operation need a sign-in per account: a
`devin-session-token$…` value has to be minted by an OAuth login, and there is no
API-key path that produces an equivalent. The proxy does that login itself, so the
CLI is not needed at runtime and not needed to add an account either. Once minted
the value is durable (no `exp` claim).

## Sign-in (minting a session token)

Two halves, both plain HTTP, and neither one involves the CLI.

**The code.** The browser is sent to the webapp's CLI continuation page:

```
{webapp}/auth/cli/continue
  ?state={22 chars of base64url}
  &prompt=select_account
  &code_challenge={base64url(sha256(verifier))}
  &code_challenge_method=S256
  &cli_pkce_marker=1
```

`prompt=select_account` is what makes the account picker appear, and
`cli_pkce_marker=1` is what makes the page print the code instead of only
redirecting. There is deliberately **no `redirect_uri`**: the site validates that
parameter against a fixed allowlist once the account picker has been passed, and
anything not on it gets a page reading `Invalid redirect URI`. The CLI's own two
shapes are visible in its binary — a loopback callback at
`http://127.0.0.1:<port>/callback`, and no `redirect_uri` at all — and only the
second is available to anything that is not that CLI. Because the check happens
after authentication, an unauthenticated request sees none of this: it is bounced
to the login page with the whole query preserved, which is how a bad redirect URI
can look fine in a test and be a dead link for a signed-in person.

**The exchange.** One anonymous Connect unary call, no `Authorization` header —
there is no credential yet:

```
POST {api_server}/exa.seat_management_pb.SeatManagementService/ExchangeDevinCLIPKCECode
content-type: application/proto
connect-protocol-version: 1

field 1 (string): the code from the page
field 2 (string): the PKCE verifier
```

The response carries the session token as a JWT; the `devin-session-token$`
marker in front of it is added by the client when the server does not send it
already. Errors come back as `{"code":"unauthenticated","message":"Invalid or
expired code…"}`, and that sentence is what the dashboard shows, because it names
the fix.

`api_server` is `api_server_url` from `credentials.toml`
(`https://server.codeium.com`). The webapp host is
`https://app.devin.ai`, both overridable by configuration.

## Framing

Streaming calls use the Connect envelope. Each frame is:

```
[flags: 1 byte][length: 4 bytes big-endian][payload: length bytes]
```

| flags  | meaning                                                    |
|--------|------------------------------------------------------------|
| `0x00` | data frame, payload is a `GetChatMessageResponse`           |
| `0x02` | end of stream, payload is a JSON object (`{}` on success)   |

Errors arrive as an end-of-stream frame carrying
`{"error":{"code":"…","message":"…"}}`, which is where the `failed_precondition`
and `unauthenticated` codes in the proxy's error mapping come from.

**A refused credential arrives this way, on an HTTP 200 response.** The status
line says success and the refusal is the first frame:

```
{"error":{"code":"unauthenticated","message":"failed to get primary API key;
 try logging out and logging in again: failed to validate Devin token: Invalid token"}}
```

**Measured.** An invalid `devin-session-token$…` value gets `200 OK` and an error
frame, not a 401. The practical consequence is that no request can be considered
healthy until a frame has been read, which is why the proxy reads the first frame
inside its account-retry loop rather than trusting the status code. A 401 status
does happen too, but it is not the only shape a refusal takes.

Unary calls (the preflights) use `Content-Type: application/proto` and send the
message bare, with no envelope.

**Connect's JSON codec is not supported** by this service; only the protobuf
codec works.

## Messages

Field numbers as observed. `GetChatMessageRequest`:

| # | field | type | notes |
|---|-------|------|-------|
| 1 | `metadata` | Metadata | **required** |
| 2 | `prompt` | string | the system prompt; 18074 bytes from the CLI |
| 3 | `chat_message_prompts` | repeated ChatMessagePrompt | **required** |
| 7 | `request_type` | enum | CLI sends `5` (CASCADE) |
| 8 | `configuration` | CompletionConfiguration | sampling parameters |
| 10 | `tools` | repeated ChatToolDefinition | 25 entries from the CLI |
| 15 | `trajectory_reference` | TrajectoryReference | a fresh one starts a new conversation |
| 16 | `cascade_id` | string | CLI sent it empty |
| 20 | `planner_mode` | enum | CLI sends `1` (DEFAULT) |
| 21 | `chat_model_uid` | string | **required**, e.g. `swe-1-6-slow` |
| 22 | `execution_id` | string | CLI sent it empty |

`Metadata`:

| # | field | notes |
|---|-------|-------|
| 1 | `ide_name` | CLI sends `"devin-cli"` |
| 2 | `extension_version` | `"3000.11.1"` |
| 3 | `api_key` | **required**; the token again |
| 4 | `locale` | `"en"` |
| 5 | `os` | `"windows"` |
| 7 | `ide_version` | `"3000.11.1"` |
| 12 | `extension_name` | `"chisel"` (the CLI's internal codename) |
| 28 | | repeated `"chisel"` |
| 30 | | packed varints, values around 10–60 (inferred: capability flags) |
| 31 | | 512-byte opaque blob, looks like an integrity hash |

`ChatMessagePrompt`:

| # | field | notes |
|---|-------|-------|
| 1 | `message_id` | UUID; the response uses `bot-<uuid>` for the assistant's |
| 2 | `source` | see below |
| 3 | `prompt` | the text |
| 6 | `tool_calls` | repeated ChatToolCall |
| 7 | `tool_call_id` | correlates a tool result with its call |
| 9 | `tool_result_is_error` | bool |
| 10 | `images` | repeated ImageData, see below |
| 11 | `thinking` | reasoning text |
| 12 | `signature` | thinking signature |
| 13 | `thinking_redacted` | bool |

`ImageData`:

| # | field | notes |
|---|-------|-------|
| 1 | `width` | varint |
| 2 | `height` | varint |
| 3 | `base64_data` | the image bytes, base64, without the `data:` prefix |
| 4 | `mime_type` | e.g. `image/png` |
| 5 | `source_path` | |
| 6 | `caption` | |

The field numbers here were wrong on the first attempt. Third-party prior art put
`base64_data` at 1 and `mime_type` at 2, and that guess fails *silently*: a
length-delimited value written into a varint field is skipped as an unknown
field, so the request goes through with no image attached. The correct layout came
from the CLI binary's own struct table, which reports `struct ImageData with 6
elements` and the order `width height base64_data mime_type source_path caption`.

**An image sent this way is accepted, but the model does not demonstrably read
it.** Colour identification measured at chance over repeated forced-choice trials
(`tools/vision-accuracy.mjs`, three solid colours, 3-way choice): **2 correct out of
11 completed trials**, against a 1-in-3 baseline. Red scored 0/3 in both runs,
answered `Blue`, `Green` and once nothing; green scored 1/3 each time. Two replies
came back empty after the client's own timeout rather than as an answer, and one
trial failed at the network level. The answers are scattered across every colour
name rather than clustered near the truth, which is the signature of guessing.

Two weaker signals point the same way. The model's own reasoning text quotes the
literal base64 payload (`Input: An image provided as a base64 data URI
(iVBORw0KGgo...)`), which suggests the backend is passing the image bytes through as
text rather than decoding them into visual tokens. And with no image at all the
model declines to name a colour, while with one attached it always names something,
so the image does influence the request without being readable.

This is worth stating as an open question rather than a settled one: the request is
well-formed and the field numbers are verified, so the likely explanations are that
this free-tier model has no usable vision, or that the backend needs the image
described in some further way that a capture would reveal. What is settled is the
practical answer — **do not build anything that depends on the model seeing an
image.**

`CompletionConfiguration`:

| # | field | type | notes |
|---|-------|------|-------|
| 1 | `num_completions` | varint | |
| 2 | `max_tokens` | varint | |
| 3 | `max_newlines` | varint | **must be non-zero** if the message is present |
| 5 | `temperature` | fixed64 (double) | |
| 7 | `top_k` | varint | **must be non-zero** if the message is present |
| 8 | `top_p` | fixed64 (double) | |

`GetChatMessageResponse`:

| # | field | notes |
|---|-------|-------|
| 1 | `message_id` | `bot-<uuid>` |
| 2 | `timestamp` | {1: seconds, 2: nanos} |
| 3 | `delta_text` | the answer, streamed in fragments |
| 4 | `delta_tokens` | |
| 5 | `stop_reason` | see below |
| 6 | `delta_tool_calls` | repeated ChatToolCall — **fragmented**, see below |
| 7 | `usage` | ModelUsageStats |
| 9 | `delta_thinking` | reasoning, streamed before the answer |
| 10 | `delta_signature` | |
| 12 | `latency` | fixed64 |
| 17 | `request_id` | trace id |
| 23 | `actual_model_uid` | resolves to `swe-1-6-slow` |

`ModelUsageStats`: `2 input_tokens`, `3 output_tokens`, `4 cache_write_tokens`,
`5 cache_read_tokens`, `9 model_uid`.

## Things that had to be discovered by experiment

### Which fields the backend actually requires

Done by replaying a captured request and deleting one field at a time
(`cmd/devin-call -replay … -drop …`), then re-encoding from scratch and
bisecting the failures.

- **Required:** `metadata` (with `api_key`), `chat_message_prompts`,
  `chat_model_uid`.
- **Optional:** `trajectory_reference`, `tools`, `prompt` (the system prompt),
  `configuration`, `cascade_id`, `execution_id`, and the opaque metadata fields
  28/30/31.

The trap: with `configuration` present, **`max_newlines` and `top_k` must not be
zero**. The CLI sends `max_newlines=400, top_k=40`, and a from-scratch request
that omitted them was rejected with `connect error invalid_argument` even though
every other field was byte-identical to a working request. This proxy therefore
always populates both.

### A fresh session id is accepted (no CLI-minted id needed)

The question this project started with. Both were tried on live requests:

- A freshly generated random `trajectory_reference.trajectory_id` — **works**.
- A freshly generated random `cascade_id` — **works**.
- Omitting `cascade_id` entirely — **works**; the backend starts a new
  trajectory.

So no, a session id minted by the CLI is not required. The CLI's own captured
request sent `cascade_id` empty and a `trajectory_reference` of
`{id: <uuid>, type: 4, field4: 14}`, which is what a new conversation looks
like.

### Reasoning cannot be turned off

Every response begins with a reasoning phase on `delta_thinking` (field 9) before
any answer text arrives on `delta_text` (field 3). It is charged to the same
`max_tokens` budget as the answer, so a small budget yields an empty answer with
`StopReason` `MAX_TOKENS` — a failure that looks like the model refusing to
speak.

Whether it is suppressible was probed deliberately
(`tools/probe-thinking.bat`), since low-latency autocomplete would benefit:

| variant | thinking produced |
|---------|-------------------|
| `request_type=5, planner_mode=1` (what the CLI sends) | 104 bytes |
| `request_type=5`, `planner_mode` omitted | 104 bytes |
| `request_type` omitted | 189 bytes |
| `request_type=1` | 175 bytes |

Reasoning happens in every case, so neither field controls it and the request has
no unmapped field left to try. The proxy works around it instead: it applies a
`min_max_tokens` floor so a client asking for 64 tokens still gets an answer.
Cost is real — a small fill-in-the-middle request that returns a three-token
answer measured **130 prompt tokens and 663 completion tokens**.

The floor's value was measured rather than guessed (`tools/budget-probe.mjs`).
Sending one question about an inline PNG at three budgets (the answer text is
incidental here — the point is the token accounting, and per the `ImageData` section
the model was not really reading the image):

| client `max_tokens` | effective | completion tokens | `finish_reason` | answer |
|---|---|---|---|---|
| 64 | 4096 (old floor) | 4096 | `length` | **empty** |
| — (floored) | 8192 (new floor) | 3308 | `stop` | present |
| 16384 | 16384 | 8046 | `stop` | verbose but present |

At the old floor of 4096 the model spent the entire budget on reasoning and
returned nothing — the worst outcome, since the quota is consumed either way. The
floor is now **8192**, which covers the observed 3308–8046 range. Because
`max_tokens` is a ceiling rather than a spend, a generous floor does not itself
cost anything: the model stops on its own.

### A prompt must not be empty

A `ChatMessagePrompt` whose `prompt` is empty is rejected with the same opaque
`The third-party model provider is experiencing issues` error as a bad `source`,
even when it carries `tool_calls`. Bisecting the message shapes
(`tools/tool-shape-probe.mjs`):

| shape | result |
|-------|--------|
| `user` + `tool` | ok |
| `user` + `assistant("text")` + `tool` | ok |
| `user` + `assistant("text", tool_calls)` + `tool` | ok |
| `user` + `assistant("", tool_calls)` + `tool` | **502** |
| `user` + `assistant("", tool_calls)` | **502** |

This matters because a tool-call-only assistant turn is exactly what an IDE sends
back after it runs a tool. The proxy substitutes a short placeholder for the empty
text rather than dropping the turn, so the tool calls stay visible to the model.

### Transport retries

`postStream` retries a dial failure and HTTP 429 up to three times with a
300 ms/900 ms backoff, rebuilding the request each attempt since the body is
consumed on the first try. It deliberately does **not** retry other non-200
statuses: the backend may already have started generating, and a retry would
spend the quota twice for one client request. A stream that dies mid-response is
not retried either, because there is no way to resume it.

Observation worth recording: long generations against
`server.codeium.com` from this network occasionally die mid-stream with
`wsarecv: A connection attempt failed`. It is a network fault, not a protocol
one — the same request succeeds on retry.

### There is no assistant role

`ChatMessagePrompt.source` accepts only a small set. Sweeping every candidate
against the live backend (`tools/sweep-assistant-source.bat`):

| source | value | result |
|--------|-------|--------|
| UNSPECIFIED | 0 | rejected |
| USER | 1 | **accepted** |
| SYSTEM | 2 | **accepted** |
| *(unnamed)* | 3 | rejected |
| TOOL | 4 | **accepted** |
| — | 5 | rejected |
| — | 6 | rejected |

Rejections come back as `connect error unknown: The third-party model provider
is experiencing issues and is currently not available` — an opaque message that
looks transient but is entirely deterministic per source value.

This matches what the CLI itself does: the captured request carried **three
prompts, all `source=USER`**, with non-user context embedded in tags inside the
prompt text:

```
<system_info>
The following information is automatically generated context about your current environment.
…today's date…
</system_info>
```

So conversation history has no dedicated role. This proxy replays assistant
turns as `USER` prompts labelled `Assistant: ` and appends a sentence to the
system prompt explaining the convention, so the model does not read its own
earlier replies as user input.

The `prompt` field itself is kept deliberately small. The backend runs a
content screen over it that refuses some text outright with an opaque
`permission_denied` (see the troubleshooting notes in the README), and most
IDEs ship system prompts their users cannot edit. So field 2 always carries
this proxy's own short instruction, and the client's system prompt rides
along as a `<system>`-tagged block inside the first `USER` turn — the same
way the CLI carries non-user context like the `<system_info>` block above.
Nothing the client sent is dropped; only its position on the wire changes.

### Tool calls arrive fragmented

`delta_tool_calls` does **not** deliver one complete tool call per frame. The
observed sequence for a single `get_weather` call:

```
frame 1: {id: "call_d702…", name: "get_weather", arguments: ""}
frame 2: {id: "",           name: "",            arguments: "{"}
frame 3: {id: "",           name: "",            arguments: "\"city\": \"Paris"}
frame 4: {id: "",           name: "",            arguments: "\""}
frame 5: {id: "",           name: "",            arguments: "}"}
```

Treating each frame as a call yields five nameless calls with truncated
arguments. Fragments must be appended by position — `openai.ToolCallAccumulator`
does this — and a fragment carrying an `id` opens a new call.

### The valid model uid

Only `swe-1-6-slow` works on a free-tier account. `swe-1-6-fast` is **refused**
with the same opaque `failed_precondition`/"third-party model provider" message,
even though the CLI's own analytics report the model name as `swe-1-6-fast`.

Consequence: model names must not be forwarded to the backend verbatim. The
proxy resolves every requested name to a known uid and only passes through the
uids in `backendModelUIDs`.

### Reasonable-looking values the backend silently rejects

Four findings from probing, all of which surface as generic errors rather than
pointing at the offending field:

- **`max_newlines` and `top_k` must be non-zero** when `configuration` is
  present, or the whole request fails with `invalid_argument`. The CLI sends
  `400` and `40`.
- **A `source` value of 3** — the natural guess for an assistant turn — is
  refused, as are 0, 5 and 6; only 1, 2 and 4 are accepted.
- **An empty `prompt` is refused** even when the prompt carries tool calls; see
  the empty-prompt section above.
- **`temperature` and `top_p` must be greater than zero.** A zero is refused with
  `400 an internal error occurred`, which is the same shape as an ordinary server
  fault and names no field. Swept with `tools/temperature-sweep.mjs`:

  | value | `temperature` | `top_p` |
  |---|---|---|
  | 0 | 400 | 400 |
  | 0.001 | ok | ok |
  | 2.0 | ok | ok |
  | 2.5 | 502 (opaque provider error) | — |

  The upper bound is 2.0 for `temperature` (2.5 fails) and 1.0 for `top_p`,
  matching OpenAI's own documented ranges. Zero is the case that matters in
  practice, because a coding IDE asking for deterministic edits sends
  `temperature: 0`; `internal/openai` clamps rather than passing it through, so
  such a request degrades to "as deterministic as the backend allows" instead of
  failing outright.

### The model catalogue is readable

`GetCliModelConfigs` is a unary call the CLI makes at startup, and its response is
the full model catalogue with per-model limits and prices: 599 entries in the
capture taken here (`captures-relay/capture-005-response.txt`), 248 when read live
on 2026-09-23, because the catalogue grows and shrinks as Devin ships models. Either
way it answers the questions a client author would otherwise guess at.
`cmd/devin-models` prints a readable summary — `-file` for a capture, `-live` for
the real call, `-available` to see only what the plan does not gate:

```
> bin\devin-models.exe -file captures-relay\capture-005-response.txt -uid swe-1-6-slow

swe-1-6-slow
  name         SWE-1.6 Slow
  aliases      swe-1p6, swe-1p5
  context      200,000 tokens
  max output   128,000 tokens
  rating       3
  tokenizer    LLAMA_WITH_SPECIAL
  capabilities [8 11 12 15 20 21 24 25]
  options      Speed
  price Input         0.5 per 1M tokens
  price Cached input  0.2 per 1M tokens
  price Output        2.5 per 1M tokens
  access       available on this plan
```

The layout, read off the wire rather than from a schema:

| `ModelConfig` # | field | notes |
|---|---|---|
| 1 | display name | |
| 3 | fixed32 | a rating; `3` for SWE-1.6 Slow, unset for others |
| 18 | varint | mirrors the context window |
| 22 | **model uid** | the string a client sends as `chat_model_uid` |
| 23 | sub-message | the model's real configuration, below |
| 30 | sub-message | UI labels, one per entry in field 2 |
| 32 | repeated sub-message | one row per priced line (Input / Cached input / Output) |
| 33 | sub-message | present only when the model needs a higher plan |

| `ModelConfig.23` # | field | notes |
|---|---|---|
| 4 | **context window** | `200000` here; the catalogue's own UI text says "1M Context" for exactly the models where this is `1000000` |
| 5 | tokenizer name | `LLAMA_WITH_SPECIAL` |
| 6 | capability flags | which bits are set differs per model; not yet mapped to meanings |
| 13 | **max output** | `128000` |
| 20 | repeated aliases | `swe-1p6`, `swe-1p5` |

Each `field 32` row carries a label (field 1), a price (field 2, `fixed32`), a unit
(field 3, the literal string `1M tokens`), two further floats (fields 4 and 5) and an
optional tag (field 8). **Only field 2 is read.** Fields 4 and 5 were first recorded
here as holding the same values for every model, i.e. plan-level; the live catalogue
disproves that — for `GPT-4.1` all three floats are equal (`2`, `2`, `2`) while for
`swe-1-6-slow` they are `0.5 / 0.1 / 20` — and nothing observed since explains what
the second and third are, so they are left unread rather than labelled. No field
states a currency either, so the price is best read as "the number the catalogue
reports per 1M tokens" rather than a dollar figure.

**The catalogue is identity-dependent, and the identity is what makes it useful.**
Asked with this proxy's own `ide_name = "devin-cli"`, `GetCliModelConfigs` answers
with a **single entry** — `MODEL_CHAT_GPT_4_1_2025_04_14`, the same chat model the
status call lists first, priced properly here at 2 / 0.5 / 8 per 1M tokens and with
capabilities `[8 11 12]`. Asked as the CLI's `"chisel"`, the same call returns the
real catalogue: **248 entries** on 2026-09-23, from `swe-1-6-slow` through the
`swe-1-7`/`swe-2` families, with every limit and price populated. So the dashboard's
Models panel and `cmd/devin-models -live` both ask as the CLI — that is the only way
to get the numbers an IDE's model picker would show — while the chat path keeps its
own identity. `-ide devin-cli` reproduces the one-entry answer.

Read live, the account's position is unambiguous: of the 248 entries **exactly one is
not plan-gated on a free account**, and it is the model this proxy defaults to.

```
> bin\devin-models.exe -live -available
live response: 135191 bytes (ide_name=chisel)
swe-1-6-slow
  name         SWE-1.6 Slow
  aliases      swe-1p6, swe-1p5
  context      200,000 tokens
  max output   128,000 tokens
  tokenizer    LLAMA_WITH_SPECIAL
  capabilities [8 11 12 15 20 21 24 25]
  options      Speed
  price Input         0.5 per 1M tokens  Higher effort consumes more tokens
  price Cached input  0.2 per 1M tokens  Higher effort consumes more tokens
  price Output        2.5 per 1M tokens  Higher effort consumes more tokens
  access       available on this plan

1 model(s) matched of 248 in the catalogue; 1 are not gated behind a higher plan
```

`swe-1-6-fast`, the other uid the proxy forwards, is in the catalogue **with the gate
block set** — which is the machine-readable version of the README's "a real uid the
proxy will forward, but the backend refuses it on a free-tier plan".

The one thing the catalogue says about free is narrower than it looks: every model,
including Pro-only ones, carries an extra `field 32` row labelled **Sidekick** with
price `0` and `field 8` = `"Free"`. SWE-1.6 Slow's Input/Cached/Output rows are not
zero and are not labelled, so its pricing is *not* free — what makes it usable on a
free-tier account is the absence of `field 33`, i.e. it is not gated behind a higher
plan.

**Capabilities are not readable from the flag bits.** Which of `8 11 12 15 20 21 24
25` means what would be a guess, so vision was tested from the outside instead:
`tools/vision-accuracy.mjs` renders solid-colour images and asks a forced-choice
question. The result was chance-level accuracy, so the model is treated as having no
usable vision and the flag bits are left unmapped rather than guessed at. See the
`ImageData` section above.
### Account identity and status are readable

The same credential answers a second service, which is what makes a multi-account
view possible without the CLI: the endpoint is reached with nothing but a stored
session token, so a saved token can be labelled and its plan read at any time.

```
POST {api_server}/exa.seat_management_pb.SeatManagementService/GetUserStatus
POST {api_server}/exa.seat_management_pb.SeatManagementService/GetCliTeamSettings
content-type: application/proto, connect-protocol-version: 1
Authorization: Basic <token>-<token>
```

**The request carries the token twice.** Its body is a `Metadata` message at field
1 — the same message `GetChatMessage` embeds there — whose field 3 is the session
token. An empty body is rejected with `400 invalid_argument`, which is not
obviously an authentication problem and is why the shape had to be captured
rather than guessed. The captured request is 982 bytes beginning `0a d3 07`
(field 1, 979 bytes) and continuing `0a 06 "chisel"` (ide name), `12 …`
(extension version), `1a bd 01 …` (the token, 189 bytes), `22 02 "en"`, `2a 07
"windows"`, `3a …` (ide version), `62 06 "chisel"`, and a 732-byte opaque field 31.

`GetUserStatusResponse` is `{ 1: user_status, 2: plan_info }`. Reading the inner
message off a live response and off the CLI's cached copy of the same call:

| field | type | meaning |
|---|---|---|
| 3 | string | display name |
| 5 | string | team id (`devin-team$account-…`) |
| 6 | enum | team membership; `2` is what an approved account reports |
| 7 | string | account email |
| 13 | message | plan block, see below |
| 33 | message | the models this account is offered; see [the model list](#the-model-list-field-33) |
| 36 | string | user id (`user-…`) |

`plan_info` (field 13) is the `PlanStatus` message, and its nested field 1 → field 2
is the plan's name (`"Free"`). The CLI binary carries the message's field names in
one concatenated string table — both the names *and*, because the table is in
declaration order, the field order:

```
PlanStatus
  plan_end                            available_flex_credits   used_flow_credits
  used_prompt_credits                 used_flex_credits        available_prompt_credits
  available_flow_credits              top_up_status            was_reduced_by_orphaned_usage
  grace_period_status                 plan_start               grace_period_end
  daily_quota_remaining_percent       weekly_quota_remaining_percent
  overage_balance_micros              daily_quota_reset_at_unix
  weekly_quota_reset_at_unix          acu_consumed             acu_limit
```

(`UserStatus` follows it in the same table, with `plan_status` among its fields.)

Two cautions on reading numbers off that list. A name whose string is byte-identical
to one elsewhere in a 190 MB binary is stored once, so the run has gaps and only the
names that are *adjacent and present* can be placed; and the run is declaration
order, not field numbers, so it needs an anchor. The anchor is the pair the CLI
prints on its main page.

That pair, read on 2026-09-23 at 17:46 local (UTC−7), is
**`41% remaining (reset in 3d 7h)`**. Field **15** reads `41` and field **18** is
`2026-09-27T08:00:00Z`, exactly 3d 07h away — so 15 is the *weekly* percentage and
18 the *weekly* reset. With that anchor the five names from
`daily_quota_remaining_percent` to `weekly_quota_reset_at_unix` are contiguous, in
that order, and land on exactly the five contiguous fields the response carries
(14, 15, 16, 17, 18), with value shapes that match their names one for one: two
small values (58, 41 — percentages), one signed value (−36850 — a micros balance),
two unix timestamps. Field 17 was already established as the daily reset by a
day-apart sample, which closes the loop. The same pair was read back out of the
CLI's own cache file (`%LOCALAPPDATA%\devin\cli\user_status.<digest>.bin`, fetched
00:46:32Z the same evening) rather than only from the proxy's own call, so the
number is known to be the one the CLI itself renders.

| field | name | live value |
|---|---|---|
| 14 | `daily_quota_remaining_percent` | 58 |
| 15 | `weekly_quota_remaining_percent` | 41 |
| 16 | `overage_balance_micros` (signed) | −36850 |
| 17 | `daily_quota_reset_at_unix` | 2026-09-24T08:00:00Z |
| 18 | `weekly_quota_reset_at_unix` | 2026-09-27T08:00:00Z |
| 8, 9 | not established (some credit counter) | 2500, 500 |

`cmd/devin-status` prints the two windows in the CLI's own phrasing —
`quota week 41% remaining (resets in 3d 7h)` — and the dashboard's Accounts pool
shows the same line per account.

Two traps worth carrying. **The percentages are pointers, not ints, on purpose:**
a proto3 field whose value is the default is left off the wire, so an account with
nothing left and an account whose window the backend did not report both arrive as
"no field" — the decoder keeps them apart (nil vs a present 0), and the page only
paints a window red on a present 0. **Field 16 is signed**: read as unsigned the
varint is ten bytes and −36850 becomes 18446744073709514766.

The proxy uses the percentages for one thing beyond display: a window reporting 0
extends a cooldown a `429` already earned, up to that window's own reset. A
*present* non-zero percentage outranks the unnamed counters, which may be status
enums that read zero on a healthy account; only when no percentage came back at all
does the older rule apply — a zero on any counter is treated as exhaustion. Both
the reset timestamps, the sign of field 16, and the absent-versus-zero distinction
are exercised by `TestDecodeAccountStatusReadsBothShapes`,
`TestAnAbsentPercentageIsNotAQuotaOfZero`, `TestAccountStatusResetDeadline` and
`TestLiveAccountStatus` (the last runs only with `DEVIN2PROXY_TEST_ACCOUNT=1`).

**`metadata.ide_name` changes the answer.** Every other metadata field equal,
`ide_name = "chisel"` — what the CLI sends — returns 251 models for this account,
while this proxy's `"devin-cli"` returns 4. Anything that wants the account as the
CLI sees it must present the CLI's identity. The plan block, the identity and the
quota counters are the same either way, so only the model list below depends on it.

#### The model list (field 33)

Field 33 is a single wrapper message. Its repeated **field 1** is one model each;
its repeated **field 2** is a set of grouping messages that label those models for
display. Read off a live free-tier response, 2026-09-23:

| path | type | meaning |
|---|---|---|
| 1.f1 | string | display name, e.g. `GPT-4.1` |
| 1.f22 | string | the uid, i.e. the value `chat_model_uid` takes (`MODEL_CHAT_GPT_4_1_2025_04_14`) |
| 1.f23 | message | the spec block, below |
| 1.f32 | message | repeated rate rows, below |
| 1.f2.f1 | varint | an internal numeric id, repeated at `1.f23.f1` (259 for GPT-4.1, 109 for GPT-4o, 217 and 234 for the two Grok models) |
| 1.f4, f5, f6, f10, f13 | varint | small enums, unmapped |
| 2.f1 | string | the group's heading: `Provider` or `Cost` |
| 2.f2.f1 | string | in the `Provider` group, the provider's name (`OpenAI`, `xAI`) |
| 2.f2.f2 | string | repeated: the display names of the models in that group |

The spec block `1.f23`:

| field | type | meaning |
|---|---|---|
| 4 | varint | context window in tokens |
| 13 | varint | maximum output in tokens |
| 17 | string | the uid again, repeating `1.f22` |
| 18 | string | the api server the model is served from (`https://server.codeium.com`) |
| 5 | string | tokenizer name (`CL100K_WITH_SPECIAL`, `LLAMA_WITH_SPECIAL`) |

and one rate row `1.f32` is field 1 the label (`Input`, `Cached input`, `Output`),
field 3 the unit, and field 7 an optional note (`Higher effort consumes more tokens`
on the thinking model). The unit is always the literal string `1M tokens` — no
number, no currency — because these are the labels the Devin app draws, not a bill.

The two token counts are what make this decode checkable rather than asserted: they
match the published figures for exactly these models, which pins field 4 and field 13
from the outside. A free-tier account's four entries:

| model | uid | context | max output |
|---|---|---|---|
| GPT-4.1 | `MODEL_CHAT_GPT_4_1_2025_04_14` | 1,047,576 | 32,768 |
| GPT-4o | `MODEL_CHAT_GPT_4O_2024_08_06` | 128,000 | 16,384 |
| xAI Grok-3 | `MODEL_XAI_GROK_3` | 131,072 | 8,192 |
| xAI Grok-3 mini Thinking | `MODEL_XAI_GROK_3_MINI_REASONING` | 131,072 | 8,192 |

**These uids are not usable on the completion endpoint.** Sending
`chat_model_uid: MODEL_CHAT_GPT_4_1_2025_04_14` — and, separately,
`MODEL_XAI_GROK_3` — to `GetChatMessage` with a valid credential returns
`permission_denied: an internal error occurred` (trace id, no frames), while
`swe-1-6-slow` streams a normal completion with the same credential and the same
client. So this list is the account's entitlement inside Devin, not a second
catalogue for this proxy to route to; `cmd/devin-call -model <uid>` reproduces both
outcomes. The decoder that reads it is in `internal/devin/status.go`, and
`TestDecodeAccountStatusModelList` covers the grouping and the optional fields.

**There is no JSON codec on this service.** A body of `{"metadata":{"apiKey":…}}`
is refused with the same `400 invalid_argument` an empty body gets, in both
snake_case and the lowerCamelCase Connect's spec prescribes, so named-field output
is not available and the protobuf layout above is the only way in.

**The CLI's cache of this call is not keyed per account.** It stores each response
at `%LOCALAPPDATA%\devin\cli\user_status.<digest>.bin` as
`{version, identity_digest, fetched_at_secs, payload}` where `payload` is
base64-encoded protobuf, and the digest is the file-name suffix. On this machine
four different digests all described the *same* account, so the digest covers the
deployment as well as the credential and must not be treated as an account key:
identify an account by the email or user id inside the payload. The cached payload
is the inner `user_status` message alone, without the field-1 wrapper, which
`cmd/devin-status` accepts alongside the live shape.

### Stop reasons

`StopNormal = 2` was observed for an ordinary end-of-turn and is absent from the
trimmed third-party schema; it is named from the capture. Mapped to OpenAI
vocabulary in `openai.FinishReason`.

A consequence worth knowing when reading usage figures: a stream that the client
abandons, or that the proxy cuts short at a `stop` sequence, never receives the
final usage frame, so token totals are unavailable for those requests. The
backend's early frames do carry a `usage` sub-message, but with only the model
uid and zero counts — reporting those as "0 tokens" would be a lie, so they are
omitted (`openai.UsageFromStats`).

## Reproducing the probe

```bat
go build -o bin\devin-probe.exe .\cmd\devin-probe
bin\devin-probe.exe -addr 127.0.0.1:8787 -relay https://server.codeium.com -out captures-relay
```

Then, in another terminal, run the CLI with `WINDSURF_API_SERVER_URL` set as
shown above. `cmd/devin-decode` replays a captured body through the typed
decoder:

```bat
go run ./cmd/devin-decode -file captures-relay\capture-018-request.txt
```

## What was deliberately not done

- No TLS interception. The environment-variable override made it unnecessary,
  and a MITM would have required installing a CA — a much bigger change to your
  machine for the same result.
- No protobuf code generation. The messages are tiny and hand-encoded in
  `internal/pb`, which keeps the module dependency-free and means the recovered
  field numbers are readable in one file.
