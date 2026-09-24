// Confirms that max_tokens caps reasoning + answer together, which is why a
// small max_tokens on an image prompt yields an empty answer: the whole budget
// goes to thinking and the answer never starts.
//
//   cd tools && node budget-probe.mjs
import OpenAI from "openai";
import zlib from "node:zlib";

const KEY = process.env.DEVIN2PROXY_API_KEY;
if (!KEY) {
  console.error("set DEVIN2PROXY_API_KEY");
  process.exit(2);
}
const client = new OpenAI({
  baseURL: process.env.DEVIN2PROXY_BASE ?? "http://127.0.0.1:8788/v1",
  apiKey: KEY,
  timeout: 240_000,
  maxRetries: 0,
});

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
function solidPNG(r, g, b, size) {
  const stride = 1 + size * 3;
  const raw = Buffer.alloc(size * stride);
  for (let y = 0; y < size; y++) {
    const off = y * stride;
    raw[off] = 0;
    for (let x = 0; x < size; x++) {
      raw[off + 1 + x * 3] = r;
      raw[off + 2 + x * 3] = g;
      raw[off + 3 + x * 3] = b;
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0);
  ihdr.writeUInt32BE(size, 4);
  ihdr[8] = 8;
  ihdr[9] = 2;
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk("IHDR", ihdr),
    pngChunk("IDAT", zlib.deflateSync(raw)),
    pngChunk("IEND", Buffer.alloc(0)),
  ]);
}

const url = "data:image/png;base64," + solidPNG(255, 0, 0, 64).toString("base64");

async function run(label, extra) {
  let content = "";
  let reasoning = "";
  let finish = null;
  let usage = null;
  try {
    const stream = await client.chat.completions.create({
      model: "swe-1-6-slow",
      stream: true,
      stream_options: { include_usage: true },
      ...extra,
      messages: [
        {
          role: "user",
          content: [
            { type: "text", text: "What single colour fills this image? (red or blue)" },
            { type: "image_url", image_url: { url } },
          ],
        },
      ],
    });
    for await (const chunk of stream) {
      content += chunk.choices?.[0]?.delta?.content ?? "";
      reasoning += chunk.choices?.[0]?.delta?.reasoning_content ?? "";
      if (chunk.choices?.[0]?.finish_reason) finish = chunk.choices[0].finish_reason;
      if (chunk.usage) usage = chunk.usage;
    }
  } catch (err) {
    console.log(`${label}: ERROR ${err?.status} ${String(err?.message).slice(0, 110)}`);
    return;
  }
  const clean = (s) => JSON.stringify(s.replace(/\s+/g, " ").trim().slice(0, 60));
  console.log(
    `${label}\n   answer=${clean(content)} reasoning=${reasoning.trim().length} chars ` +
      `finish=${finish} usage=${JSON.stringify(usage)}`,
  );
}

// 64 is floored to the configured minimum by the proxy, so this shows what the
// floor actually delivers.
await run("max_tokens=64  (floored by proxy)", { max_tokens: 64 });
await run("max_tokens=16384", { max_tokens: 16384 });
await run("max_tokens omitted (proxy default)", {});
