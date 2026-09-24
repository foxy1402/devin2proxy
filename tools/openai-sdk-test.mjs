// Compatibility check with the official OpenAI Node SDK, i.e. the client
// library a real consumer of this proxy would use.
import OpenAI from "openai";

// Overridable, matching ide-sim-test.mjs, so the suite can be pointed at an
// instance running a particular configuration (a multi-account pool, say)
// without editing the file.
const BASE = process.env.DEVIN2PROXY_BASE ?? "http://127.0.0.1:8788/v1";
const KEY = process.env.DEVIN2PROXY_API_KEY ?? "sk-devin-localtest123";

const client = new OpenAI({ baseURL: BASE, apiKey: KEY, timeout: 180_000 });

let failures = 0;
function check(name, ok, detail) {
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? "  " + detail : ""}`);
  if (!ok) failures++;
}

// 1. models.list
const models = await client.models.list();
check("models.list", models.data.length > 0, models.data.map((m) => m.id).join(","));

// 2. non-streaming chat
const chat = await client.chat.completions.create({
  model: "swe-1.6",
  messages: [
    { role: "system", content: "Answer with a single word when possible." },
    { role: "user", content: "Reply with exactly the word: pong" },
  ],
});
const text = chat.choices[0].message.content.trim();
check("chat.completions.create", /pong/i.test(text), JSON.stringify(text));
check("usage populated", (chat.usage?.total_tokens ?? 0) > 0, JSON.stringify(chat.usage));
check("id has chatcmpl prefix", chat.id.startsWith("chatcmpl-"), chat.id);

// 3. streaming chat
let streamed = "";
let sawFinish = false;
let chunks = 0;
const stream = await client.chat.completions.create({
  model: "swe",
  stream: true,
  messages: [{ role: "user", content: "Count from 1 to 3, one number per line." }],
});
for await (const chunk of stream) {
  chunks++;
  const choice = chunk.choices[0];
  streamed += choice?.delta?.content ?? "";
  if (choice?.finish_reason) sawFinish = true;
}
check("stream assembled content", /\d/.test(streamed), JSON.stringify(streamed));
check("stream saw finish_reason", sawFinish, `${chunks} chunks`);

// 4. tools
const toolResp = await client.chat.completions.create({
  model: "swe",
  messages: [{ role: "user", content: "What is the weather in Paris? Use the get_weather tool." }],
  tools: [
    {
      type: "function",
      function: {
        name: "get_weather",
        description: "Get the current weather for a city",
        parameters: {
          type: "object",
          properties: { city: { type: "string" } },
          required: ["city"],
        },
      },
    },
  ],
});
const calls = toolResp.choices[0].message.tool_calls ?? [];
let argsOK = false;
if (calls.length === 1) {
  try {
    argsOK = JSON.parse(calls[0].function.arguments).city === "Paris";
  } catch {
    argsOK = false;
  }
}
check(
  "tool call parsed by SDK",
  calls.length === 1 && calls[0].function.name === "get_weather" && argsOK,
  JSON.stringify(calls),
);
check("finish_reason tool_calls", toolResp.choices[0].finish_reason === "tool_calls", toolResp.choices[0].finish_reason);

// 5. bad API key must be rejected with 401
const badClient = new OpenAI({ baseURL: BASE, apiKey: "sk-devin-wrong", timeout: 30_000 });
try {
  await badClient.chat.completions.create({
    model: "swe",
    messages: [{ role: "user", content: "hi" }],
  });
  check("401 on bad key", false, "request unexpectedly succeeded");
} catch (err) {
  check("401 on bad key", err?.status === 401, `status=${err?.status} name=${err?.name}`);
}

// 6. temperature 0 must work. The backend rejects a zero temperature with a 400
// that names no field, and IDEs send 0 whenever they want deterministic edits, so
// the proxy clamps it. Without that clamp this is a hard failure for real clients.
try {
  const cold = await client.chat.completions.create({
    model: "swe",
    temperature: 0,
    max_tokens: 32,
    messages: [{ role: "user", content: "Reply with the single word OK." }],
  });
  check("temperature 0 accepted", String(cold.choices[0].message.content).trim().length > 0, JSON.stringify(String(cold.choices[0].message.content).slice(0, 20)));
} catch (err) {
  check("temperature 0 accepted", false, `status=${err?.status} ${String(err?.message).slice(0, 80)}`);
}

console.log(failures === 0 ? "\nALL CHECKS PASSED" : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
