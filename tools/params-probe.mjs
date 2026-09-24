// Answers the "do my client's model settings actually reach the model" question
// empirically. A coding IDE lets you set a context window, an output limit and
// sometimes extra knobs; this checks which of them the proxy acts on, which it
// silently drops, and which the backend rejects.
//
//   cd tools && node params-probe.mjs
import OpenAI from "openai";

const KEY = process.env.DEVIN2PROXY_API_KEY;
if (!KEY) {
  console.error("set DEVIN2PROXY_API_KEY");
  process.exit(2);
}
const BASE = process.env.DEVIN2PROXY_BASE ?? "http://127.0.0.1:8788/v1";
const client = new OpenAI({ baseURL: BASE, apiKey: KEY, timeout: 240_000, maxRetries: 0 });

const ASK = [{ role: "user", content: "Reply with the single word OK and nothing else." }];

async function attempt(label, note, body) {
  try {
    const resp = await client.chat.completions.create({ model: "swe-1-6-slow", messages: ASK, ...body });
    const text = String(resp.choices?.[0]?.message?.content ?? "").replace(/\s+/g, " ").slice(0, 40);
    const usage = resp.usage ? `${resp.usage.completion_tokens} completion tokens` : "no usage";
    console.log(`ACCEPTED  ${label}\n          ${note}\n          -> ${resp.choices.length} choice(s), ${usage}, text=${JSON.stringify(text)}`);
  } catch (err) {
    const detail = String(err?.message ?? "").replace(/\s+/g, " ").slice(0, 150);
    console.log(`REJECTED  ${label}\n          ${note}\n          -> status=${err?.status} ${detail}`);
  }
}

console.log("=== parameters an IDE may send ===");

// 1. Unknown / non-standard fields. Some clients send these (Continue sends
//    num_ctx-ish knobs, others send top_k); the question is whether they are
//    rejected, silently dropped, or acted on.
await attempt(
  "unknown extra params",
  "{ context_window: 200000, num_ctx: 8192, top_k: 5, seed: 42, logprobs: true, reasoning_effort: 'low' }",
  {
    context_window: 200000,
    num_ctx: 8192,
    top_k: 5,
    seed: 42,
    logprobs: true,
    reasoning_effort: "low",
    max_tokens: 32,
  },
);

// 2. Output limit. The proxy floors max_tokens, so a small client limit should
//    NOT come back as a short generation ceiling.
await attempt("tiny output limit", "max_tokens: 16", { max_tokens: 16 });

// 3. Output limit above the model's advertised ceiling.
await attempt("huge output limit", "max_tokens: 500000", { max_tokens: 500000 });

// 4. Sampling knobs inside and outside the documented OpenAI ranges.
await attempt("temperature in range", "temperature: 0", { temperature: 0, max_tokens: 32 });
await attempt("temperature out of range", "temperature: 2.5", { temperature: 2.5, max_tokens: 32 });
await attempt("top_p in range", "top_p: 0.1", { top_p: 0.1, max_tokens: 32 });

// 5. n > 1. The backend has one completion per request; the question is whether
//    the proxy reports that honestly or fabricates choices.
await attempt("multiple choices", "n: 3", { n: 3, max_tokens: 32 });

// 6. Fields with no backend equivalent.
await attempt("no-backend-equivalent fields", "seed, response_format, frequency_penalty, presence_penalty", {
  seed: 7,
  response_format: { type: "text" },
  frequency_penalty: 0.5,
  presence_penalty: 0.5,
  max_tokens: 32,
});

// 7. Legacy surface: /v1/completions with the same knobs.
try {
  const resp = await client.completions.create({
    model: "swe-1-6-slow",
    prompt: "1 2 3",
    max_tokens: 16,
    temperature: 2.5,
    top_p: 0.5,
    n: 2,
    extra_body: { context_window: 200000 },
  });
  console.log(`ACCEPTED  legacy /v1/completions with odd params\n          -> ${resp.choices.length} choice(s), text=${JSON.stringify(String(resp.choices[0].text).slice(0, 40))}`);
} catch (err) {
  console.log(`REJECTED  legacy /v1/completions with odd params\n          -> status=${err?.status} ${String(err?.message).slice(0, 120)}`);
}
