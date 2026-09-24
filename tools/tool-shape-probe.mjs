// Bisects the deterministic 502 on tool-call round 2 (assistant turn carrying
// tool_calls followed by a tool result). Each case is one distinct message
// shape, so whichever one fails names the field the backend rejects.
import OpenAI from "openai";

const KEY = process.env.DEVIN2PROXY_API_KEY;
if (!KEY) {
  console.error("set DEVIN2PROXY_API_KEY");
  process.exit(2);
}
const client = new OpenAI({
  baseURL: process.env.DEVIN2PROXY_BASE ?? "http://127.0.0.1:8788/v1",
  apiKey: KEY,
  timeout: 180_000,
  maxRetries: 0,
});

const TOOLS = [
  {
    type: "function",
    function: {
      name: "read_file",
      description: "Read a file from the workspace",
      parameters: { type: "object", properties: { path: { type: "string" } }, required: ["path"] },
    },
  },
];

const CALL = {
  id: "call_abc123",
  type: "function",
  function: { name: "read_file", arguments: '{"path":"src/app.py"}' },
};

async function run(name, messages) {
  try {
    const resp = await client.chat.completions.create({
      model: "swe-1-6-slow",
      max_tokens: 256,
      messages,
      tools: TOOLS,
    });
    const m = resp.choices[0].message;
    const txt = String(m.content ?? "").replace(/\s+/g, " ").slice(0, 70);
    console.log(`OK    ${name}  finish=${resp.choices[0].finish_reason} text=${JSON.stringify(txt)}`);
  } catch (err) {
    console.log(`FAIL  ${name}  ${err?.status} ${String(err?.message).replace(/\s+/g, " ").slice(0, 100)}`);
  }
}

const ASK = [{ role: "user", content: "Read the file src/app.py using the read_file tool." }];

await run("A user+tool (no assistant turn)", [...ASK, { role: "tool", tool_call_id: CALL.id, content: "print(1)" }]);
await run("B assistant(text)+tool", [
  ...ASK,
  { role: "assistant", content: "I will read the file." },
  { role: "tool", tool_call_id: CALL.id, content: "print(1)" },
]);
await run("C assistant(text,calls)+tool", [
  ...ASK,
  { role: "assistant", content: "I will read the file.", tool_calls: [CALL] },
  { role: "tool", tool_call_id: CALL.id, content: "print(1)" },
]);
await run("D assistant('',calls)+tool", [
  ...ASK,
  { role: "assistant", content: "", tool_calls: [CALL] },
  { role: "tool", tool_call_id: CALL.id, content: "print(1)" },
]);
await run("E assistant('',calls) only", [...ASK, { role: "assistant", content: "", tool_calls: [CALL] }]);
await run("F assistant(calls) no content key", [...ASK, { role: "assistant", tool_calls: [CALL] }]);
await run("G user+assistant(text) only", [...ASK, { role: "assistant", content: "I will read the file." }]);
await run("H tool result first", [
  { role: "tool", tool_call_id: CALL.id, content: "print(1)" },
  ...ASK,
]);
