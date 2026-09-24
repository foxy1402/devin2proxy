// Measures how reliably the backend can identify an inline image, rather than
// trusting a single sample. One red PNG answered "Blue", which is not something a
// pass/fail check on one request can distinguish from a genuine capability.
//
// Uses a 3-way forced choice over solid colours, so a model that cannot see the
// image at all scores about 33%. Each call is a separate request, so the score is
// a real accuracy figure rather than a coin flip dressed up as a test.
//
//   cd tools && node vision-accuracy.mjs
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
  timeout: 540_000,
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
  const stride = size * 3;
  const raw = Buffer.alloc(size * (1 + stride));
  for (let y = 0; y < size; y++) {
    const off = y * (1 + stride);
    raw[off] = 0;
    for (let x = 0; x < size; x++) {
      const p = off + 1 + x * 3;
      raw[p] = r;
      raw[p + 1] = g;
      raw[p + 2] = b;
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

const QUESTION = "What single colour fills this image: red, green, or blue? Answer with one word.";

const CASES = [
  { name: "red", rgb: [255, 0, 0] },
  { name: "green", rgb: [0, 128, 0] },
  { name: "blue", rgb: [0, 0, 255] },
];

const TRIALS = Number(process.env.TRIALS ?? 3);
const SIZE = Number(process.env.SIZE ?? 128);

async function ask(question, png, withImage) {
  const content = [{ type: "text", text: question }];
  if (withImage) {
    content.push({ type: "image_url", image_url: { url: "data:image/png;base64," + png.toString("base64") } });
  }
  const stream = await client.chat.completions.create({
    model: "swe-1-6-slow",
    stream: true,
    messages: [{ role: "user", content }],
  });
  let answer = "";
  for await (const chunk of stream) answer += chunk.choices?.[0]?.delta?.content ?? "";
  return answer.replace(/\s+/g, " ").trim();
}

const tally = new Map();
let correct = 0;
let total = 0;

for (const c of CASES) {
  const png = solidPNG(...c.rgb, SIZE);
  const answers = [];
  for (let i = 0; i < TRIALS; i++) {
    const started = Date.now();
    let answer;
    try {
      answer = await ask(QUESTION, png, true);
    } catch (err) {
      answer = `ERROR ${err?.status} ${String(err?.message).slice(0, 60)}`;
    }
    const secs = ((Date.now() - started) / 1000).toFixed(0);
    const hit = new RegExp(`\\b${c.name}\\b`, "i").test(answer);
    if (!/^ERROR/.test(answer)) {
      total++;
      if (hit) correct++;
    }
    answers.push(`${hit ? "ok" : "MISS"} ${JSON.stringify(answer.slice(0, 28))} ${secs}s`);
  }
  console.log(`${c.name.padEnd(6)} ${answers.join("  |  ")}`);
}

console.log(`\nwith image   : ${correct}/${total} correct`);

// Control: the same question with no image. A model that is genuinely reading the
// image should answer differently here, since there is nothing to read.
const control = [];
for (let i = 0; i < TRIALS; i++) {
  try {
    control.push(JSON.stringify((await ask(QUESTION, null, false)).slice(0, 30)));
  } catch (err) {
    control.push(`ERROR ${err?.status}`);
  }
}
console.log(`no image     : ${control.join("  |  ")}`);
console.log(`\nchance for a 3-way forced choice is 33%. An accuracy near that means the`);
console.log(`model is guessing rather than reading the pixels.`);
