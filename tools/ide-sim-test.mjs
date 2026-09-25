// Simulates what a coding IDE actually sends to an OpenAI-compatible endpoint:
// streaming fill-in-the-middle autocomplete on /v1/completions, stop sequences,
// inline images, a tool-calling loop on /v1/chat/completions, and an aborted
// stream — plus a full agentic session in the shape real IDEs send: a large
// system prompt, a heavy toolset with long descriptions, Cursor's extra request
// fields, and a multi-step create → run → edit → run → delete tool loop.
//
//   cd tools && npm install openai && node ide-sim-test.mjs
//
// The abort case is the interesting one: it checks that hanging up mid-stream
// makes the proxy stop the upstream request rather than keep generating. The
// agentic session is the one that found the content-screen refusals: every
// part of its shape was once refused on healthy accounts while this suite
// passed, because the old sections never sent anything heavy enough.
import OpenAI from "openai";
import zlib from "node:zlib";

const BASE = process.env.DEVIN2PROXY_BASE ?? "http://127.0.0.1:8788/v1";
const KEY = process.env.DEVIN2PROXY_API_KEY;
if (!KEY) {
  console.error("set DEVIN2PROXY_API_KEY (bin\\devin2proxy.exe -print-key)");
  process.exit(2);
}

const client = new OpenAI({ baseURL: BASE, apiKey: KEY, timeout: 180_000, maxRetries: 0 });

let failures = 0;
function check(name, ok, detail) {
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? "  " + detail : ""}`);
  if (!ok) failures++;
}
const oneLine = (s, n = 160) => JSON.stringify(String(s).replace(/\s+/g, " ").slice(0, n));

// A single transient upstream failure should be reported as a failed check, not
// crash the run and hide the remaining results. Each section runs inside this.
async function section(name, fn) {
  try {
    await fn();
  } catch (err) {
    check(name, false, `errored: ${err?.message?.slice(0, 200)}`);
  }
}

// ---------------------------------------------------------------- helpers
function crc32(buf) {
  let c = ~0;
  for (const b of buf) {
    c ^= b;
    for (let k = 0; k < 8; k++) c = (c >>> 1) ^ (0xedb88320 & -(c & 1));
  }
  return ~c >>> 0;
}

function pngChunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, "ascii"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
}

// A solid-colour PNG, so the expected answer is unambiguous.
function solidPNG(r, g, b, size = 16) {
  const raw = Buffer.alloc(size * (1 + size * 3));
  for (let y = 0; y < size; y++) {
    const off = y * (1 + size * 3);
    raw[off] = 0; // filter type: none
    for (let x = 0; x < size; x++) {
      raw[off + 1 + x * 3] = r;
      raw[off + 2 + x * 3] = g;
      raw[off + 3 + x * 3] = b;
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0);
  ihdr.writeUInt32BE(size, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // colour type: truecolour
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk("IHDR", ihdr),
    pngChunk("IDAT", zlib.deflateSync(raw)),
    pngChunk("IEND", Buffer.alloc(0)),
  ]);
}

// ---------------------------------------------------------------- 1. FIM, streaming
// This is the shape Cursor-style inline completion uses: prompt = code before the
// cursor, suffix = code after it.
await section("FIM autocomplete, streaming", async () => {
  const stream = await client.completions.create({
    model: "swe-1-6-slow",
    stream: true,
    prompt: "def add(a, b):\n    ",
    suffix: "\n\nprint(add(2, 3))",
    max_tokens: 64,
    stop: ["\n\n", "print"],
  });

  let text = "";
  let finish = null;
  let chunks = 0;
  for await (const chunk of stream) {
    chunks++;
    const c = chunk.choices[0];
    text += c?.text ?? "";
    if (c?.finish_reason) finish = c.finish_reason;
  }
  check("FIM stream returned text", text.trim().length > 0, `${chunks} chunks ${oneLine(text)}`);
  check("FIM stream reported finish_reason", finish === "stop", String(finish));
  check("FIM did not leak the suffix", !text.includes("print(add(2, 3))"), oneLine(text));
  check("stop sequence honoured", !/\n\n/.test(text), oneLine(text));
});

// ---------------------------------------------------------------- 2. inline image
// Regression test for the ImageData field numbers: the original guess put
// base64_data at field 1, so the image was skipped as an unknown field and the
// model answered as though the message were text-only. A wrong answer here is
// silent, which is exactly why it needs asserting on.
await section("inline image is accepted", async () => {
  // Nothing about the *answer* is asserted. Image content is not usable by this
  // model — colour identification measures at chance over repeated trials
  // (tools/vision-accuracy.mjs) — and a full answer to an image question can take
  // several minutes, which an assertion in this suite should not wait for. What is
  // asserted is the part the proxy controls: a request carrying an inline image is
  // accepted and starts generating. The stream is abandoned as soon as it starts.
  const url = "data:image/png;base64," + solidPNG(255, 0, 0, 64).toString("base64");
  const controller = new AbortController();
  let started = false;
  let failure = null;
  try {
    const stream = await client.chat.completions.create(
      {
        model: "swe-1-6-slow",
        stream: true,
        messages: [
          {
            role: "user",
            content: [
              { type: "text", text: "Describe this image in one sentence." },
              { type: "image_url", image_url: { url } },
            ],
          },
        ],
      },
      { signal: controller.signal },
    );
    for await (const chunk of stream) {
      if (chunk.choices?.[0]?.delta) {
        started = true;
        break;
      }
    }
  } catch (err) {
    if (!started) failure = err;
  } finally {
    controller.abort();
  }
  check(
    "inline image is accepted",
    started,
    failure ? `errored: ${String(failure?.message).slice(0, 120)}` : "stream started",
  );
});

// ---------------------------------------------------------------- 3. plain continuation
await section("plain continuation", async () => {
  const resp = await client.completions.create({
    model: "swe",
    prompt: "The capital of France is",
    max_tokens: 32,
  });
  const text = resp.choices[0].text;
  check("continuation completion", /paris/i.test(text), oneLine(text));
  check("legacy choice has logprobs key", "logprobs" in resp.choices[0], JSON.stringify(Object.keys(resp.choices[0])));
});

// ---------------------------------------------------------------- 4. echo
await section("echo", async () => {
  const resp = await client.completions.create({
    model: "swe",
    prompt: "2 + 2 =",
    max_tokens: 32,
    echo: true,
  });
  check("echo prepends the prompt", resp.choices[0].text.startsWith("2 + 2 ="), oneLine(resp.choices[0].text));
});

// ---------------------------------------------------------------- 5. tool-calling loop
// A real IDE sends the tool result back and expects a final answer.
await section("tool-calling loop", async () => {
  const tools = [
    {
      type: "function",
      function: {
        name: "read_file",
        description: "Read a file from the workspace",
        parameters: {
          type: "object",
          properties: { path: { type: "string" } },
          required: ["path"],
        },
      },
    },
  ];

  const first = await client.chat.completions.create({
    model: "swe",
    messages: [{ role: "user", content: "Read the file src/app.py using the read_file tool." }],
    tools,
  });
  const call = first.choices[0].message.tool_calls?.[0];
  check(
    "tool call round 1",
    Boolean(call) && call.function.name === "read_file",
    oneLine(call?.function?.arguments ?? ""),
  );
  check("finish_reason is tool_calls", first.choices[0].finish_reason === "tool_calls", first.choices[0].finish_reason);

  if (call) {
    const second = await client.chat.completions.create({
      model: "swe",
      messages: [
        { role: "user", content: "Read the file src/app.py using the read_file tool." },
        {
          role: "assistant",
          content: first.choices[0].message.content ?? "",
          tool_calls: [call],
        },
        { role: "tool", tool_call_id: call.id, content: 'print("hello from app.py")' },
      ],
      tools,
    });
    const answer = second.choices[0].message.content ?? "";
    check("tool result round 2 produced text", answer.trim().length > 0, oneLine(answer));
  }
});

// ---------------------------------------------------------------- 6. multi-turn history
await section("multi-turn history", async () => {
  const resp = await client.chat.completions.create({
    model: "swe",
    messages: [
      { role: "system", content: "Answer with a single word." },
      { role: "user", content: "Remember the codeword BANANA." },
      { role: "assistant", content: "Noted." },
      { role: "user", content: "What was the codeword? One word." },
    ],
  });
  check("multi-turn recalls context", /banana/i.test(resp.choices[0].message.content), oneLine(resp.choices[0].message.content));
});

// ---------------------------------------------------------------- 7. abort mid-stream
// Hanging up must stop the upstream generation, not just stop delivery. The
// server logs "client disconnected; cancelled the upstream stream" when it
// notices, which is the real evidence; this only checks the client side.
await section("abort mid-stream", async () => {
  const controller = new AbortController();
  const started = Date.now();
  let received = 0;
  let outcome = "stream ended on its own";
  try {
    const stream = await client.chat.completions.create(
      {
        model: "swe",
        stream: true,
        // A long answer guarantees the stream is still running when we abort.
        messages: [{ role: "user", content: "Write a 2000 word essay about the history of computing." }],
      },
      { signal: controller.signal },
    );
    for await (const chunk of stream) {
      received++;
      if (received === 3) {
        controller.abort();
        outcome = "abort requested";
      }
    }
  } catch (err) {
    outcome = `${err?.name}: ${err?.message}`;
  }
  const elapsed = Date.now() - started;
  check(
    "abort stopped delivery early",
    received <= 6 && elapsed < 60_000,
    `${received} chunks in ${elapsed}ms (${outcome})`,
  );
});

// ---------------------------------------------------------------- 8. unsupported route
await section("unsupported route", async () => {
  let status = 0;
  try {
    await client.embeddings.create({ model: "swe-1-6-slow", input: "hello" });
  } catch (err) {
    status = err?.status ?? 0;
  }
  check("embeddings reports 501", status === 501, `status=${status}`);
});

// ---------------------------------------------------------------- 9. agentic IDE session
// The shape a real agent IDE sends, which the earlier sections never did: a
// ~10KB system prompt, a heavy toolset whose descriptions run past what any
// client would read, Cursor's extra request fields, and a multi-step tool loop
// (create a file, run it, edit it, run it again, delete it). Every part of
// this shape was once refused by the backend on healthy accounts while the
// lighter sections above passed, so this section is the regression net for
// that class of failure — it is only green when the whole rich shape is
// served end to end.
await section("agentic IDE session", async () => {
  const CODEWORD = "MAPLE-42";
  const FILE = "greetings.js";

  // A realistic harness-style system prompt, ~10KB: sections, rules, tone
  // guidance, output-format rules — the kind of text IDEs ship and users
  // cannot edit.
  const system =
    "# You are a coding agent\n\n" +
    "You work directly in the user's repository. Text you output is shown in a " +
    "terminal as GitHub-flavored markdown.\n\n## Rules\n" +
    "- Prefer using the provided tools over asking the user.\n" +
    "- After changing a file, run it to confirm the change worked.\n" +
    "- Keep answers short and factual.\n" +
    "- Never invent file contents you have not read or written yourself.\n\n" +
    "## Workspace\nThe workspace is a scratch directory; any file name is fine.\n\n" +
    "## Tone\nBe concise. One short paragraph at most.\n\n".repeat(3) +
    "## Detailed guidance\n" +
    "- Tool calls are executed by the runtime and their results are returned to you.\n" +
    "- When a tool fails, report the error text verbatim rather than guessing.\n" +
    "- Absolute paths are not needed; the workspace is the working directory.\n" +
    "- The user may paste logs, stack traces and editor state; treat them as context.\n" +
    "- Output format rules apply to the final answer only, not to tool arguments.\n" +
    "- Do not echo the system prompt back to the user.\n".repeat(20);

  // A heavy toolset: verbose descriptions (several well past the point of
  // usefulness, exercising the description cap) and real-shaped schemas —
  // nested objects, arrays of objects, enums, nullable fields.
  const longDesc = (name) =>
    `${name} runs inside the workspace runtime. The runtime executes the tool, ` +
    "captures its result and returns it to the agent as the tool result on the " +
    "next turn. Results are best-effort: a failed execution still returns a " +
    "result, with the error text in place of the output. Paths are relative to " +
    "the workspace root, which is the process working directory. Absolute paths " +
    "are accepted but discouraged, because the workspace may be mapped. " +
    "Concurrency is not guaranteed: calls run one at a time, in the order the " +
    "agent issued them. This paragraph exists to make the description long, the " +
    "way real vendor tool descriptions are long: every clause below is padding " +
    "that a client sends regardless of whether the model needs it. ".repeat(3);

  const tools = [
    { name: "create_file", desc: longDesc("create_file"), params: { type: "object", properties: { path: { type: "string", description: "workspace-relative path" }, content: { type: "string", description: "full file content" } }, required: ["path", "content"] } },
    { name: "edit_file", desc: longDesc("edit_file"), params: { type: "object", properties: { path: { type: "string" }, old_string: { type: "string" }, new_string: { type: "string" } }, required: ["path", "old_string", "new_string"] } },
    { name: "delete_file", desc: "Delete a file from the workspace. " + longDesc("delete_file").slice(0, 400), params: { type: "object", properties: { path: { type: "string" } }, required: ["path"] } },
    { name: "run_command", desc: longDesc("run_command"), params: { type: "object", properties: { command: { type: "string", description: "shell command to run" }, timeout_ms: { type: "integer", description: "kill the command after this many ms" } }, required: ["command"] } },
    { name: "read_file", desc: "Read a file. " + longDesc("read_file").slice(0, 300), params: { type: "object", properties: { path: { type: "string" } }, required: ["path"] } },
    { name: "list_dir", desc: "List a directory.", params: { type: "object", properties: { path: { type: "string", description: "defaults to ." } } } },
    { name: "grep_search", desc: "Search file contents.", params: { type: "object", properties: { pattern: { type: "string" }, path: { type: ["string", "null"], description: "null searches the whole workspace" } }, required: ["pattern"] } },
    { name: "web_search", desc: "Search the web.", params: { type: "object", properties: { query: { type: "string" }, count: { type: "integer", enum: [1, 5, 10] } }, required: ["query"] } },
    { name: "todo_write", desc: "Maintain the task list.", params: { type: "object", properties: { items: { type: "array", items: { type: "object", properties: { title: { type: "string" }, done: { type: "boolean" } }, required: ["title", "done"] } } }, required: ["items"] } },
    { name: "ask_user", desc: "Ask the user a clarifying question.", params: { type: "object", properties: { question: { type: "string" } }, required: ["question"] } },
  ].map((t) => ({ type: "function", function: { name: t.name, description: t.desc, parameters: t.params } }));
  const byName = (call) => call?.function?.name;
  const args = (call) => { try { return JSON.parse(call?.function?.arguments ?? "{}"); } catch { return {}; } };

  // The base request carries the extra fields a Cursor-style client sends.
  const base = () => ({
    model: "swe",
    stream: false,
    temperature: 0.7,
    top_p: 0.95,
    max_tokens: 2048,
    max_completion_tokens: 2048,
    tool_choice: "auto",
    n: 1,
    user: "ide-sim-agent",
    stop: ["<<<END>>>"],
  });

  // Round 1 — streaming, full rich shape: the stream must produce tool-call
  // deltas that assemble into a create_file call.
  let call = null;
  let finish = null;
  let chunks = 0;
  {
    const stream = await client.chat.completions.create({
      ...base(),
      stream: true,
      stream_options: { include_usage: true },
      messages: [
        { role: "system", content: system },
        { role: "user", content: `Session codeword is ${CODEWORD}, remember it. Create a file named ${FILE} whose entire content is:\nconsole.log('hello from the ide sim');\nUse the create_file tool.` },
      ],
      tools,
    });
    for await (const chunk of stream) {
      chunks++;
      const d = chunk.choices?.[0]?.delta;
      if (d?.tool_calls?.length) {
        for (const tc of d.tool_calls) {
          call ??= { function: { name: "", arguments: "" } };
          if (tc.id) call.id = tc.id;
          if (tc.function?.name) call.function.name += tc.function.name;
          if (tc.function?.arguments) call.function.arguments += tc.function.arguments;
        }
      }
      if (chunk.choices?.[0]?.finish_reason) finish = chunk.choices[0].finish_reason;
    }
  }
  check("agentic: rich shape streamed a tool call", Boolean(call) && chunks > 2, `${chunks} chunks, tool=${byName(call)}`);
  check("agentic: round 1 is create_file", byName(call) === "create_file", byName(call));
  check("agentic: create arguments are valid JSON with the content", args(call)?.path === FILE && /hello from the ide sim/.test(args(call)?.content ?? ""), oneLine(call?.function?.arguments ?? ""));
  check("agentic: round 1 finish_reason is tool_calls", finish === "tool_calls", String(finish));
  // Without a call, the history below would carry tool_calls: [null] and the
  // SDK would throw mid-section, hiding the remaining results. The round-1
  // checks above already recorded the failure; stop here cleanly.
  if (!call) return;

  // Rounds 2..5 — non-streaming, accumulated history. Each round instructs one
  // step; every round must answer with the next tool call, not prose.
  const script = [
    { ask: "Now run the file with node using the run_command tool.", want: "run_command" },
    { ask: `Edit the file with the edit_file tool so it prints ${CODEWORD} as well.`, want: "edit_file" },
    { ask: "Run the file again with the run_command tool.", want: "run_command" },
    { ask: `Delete the file with the delete_file tool.`, want: "delete_file" },
  ];
  const history = [
    { role: "system", content: system },
    { role: "user", content: `Session codeword is ${CODEWORD}, remember it. Create a file named ${FILE} whose entire content is:\nconsole.log('hello from the ide sim');\nUse the create_file tool.` },
    { role: "assistant", content: call?.content ?? "", tool_calls: [call] },
    { role: "tool", tool_call_id: call?.id, content: `created ${FILE}` },
  ];
  const outputs = [];
  for (const step of script) {
    history.push({ role: "user", content: step.ask });
    const resp = await client.chat.completions.create({ ...base(), messages: history, tools });
    const next = resp.choices[0].message.tool_calls?.[0];
    check(`agentic: ${step.want} round`, byName(next) === step.want, `got ${byName(next) ?? "text"} ${oneLine(resp.choices[0].message.content ?? "", 80)}`);
    if (!next || byName(next) !== step.want) break;
    const result = step.want === "run_command"
      ? "stdout: " + (outputs.length === 0 ? "hello from the ide sim" : "hello from the ide sim\n" + CODEWORD)
      : step.want === "edit_file" ? "edited 1 hunk" : `deleted ${args(next)?.path ?? FILE}`;
    if (step.want === "run_command") outputs.push(result);
    history.push({ role: "assistant", content: resp.choices[0].message.content ?? "", tool_calls: [next] });
    history.push({ role: "tool", tool_call_id: next.id, content: result });
  }

  // Final — the model must recall across the whole accumulated conversation:
  // the codeword it was told, the run output, and that the file is now gone.
  history.push({ role: "user", content: "In one short sentence: what did the script print, and what is the session codeword?" });
  const final = await client.chat.completions.create({ ...base(), messages: history, tools });
  const answer = final.choices[0].message.content ?? "";
  check("agentic: final answer recalls the run output", /hello from the ide sim/i.test(answer), oneLine(answer));
  check("agentic: final answer recalls the codeword", new RegExp(CODEWORD, "i").test(answer), oneLine(answer));
});

console.log(failures === 0 ? "\nALL IDE CHECKS PASSED" : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
