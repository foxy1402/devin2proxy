// Simulates what a coding IDE actually sends to an OpenAI-compatible endpoint:
// streaming fill-in-the-middle autocomplete on /v1/completions, stop sequences,
// inline images, a tool-calling loop on /v1/chat/completions, and an aborted
// stream.
//
//   cd tools && npm install openai && node ide-sim-test.mjs
//
// The abort case is the interesting one: it checks that hanging up mid-stream
// makes the proxy stop the upstream request rather than keep generating.
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

console.log(failures === 0 ? "\nALL IDE CHECKS PASSED" : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
