// Finds the range of `temperature` the backend accepts. Coding IDEs default to
// temperature 0 for deterministic edits, and that value is rejected outright, so
// the proxy needs to know the safe boundary to clamp to.
//
//   cd tools && node temperature-sweep.mjs
import OpenAI from "openai";

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

const ASK = [{ role: "user", content: "Reply with the single word OK and nothing else." }];

for (const t of [0, 0.001, 0.01, 0.05, 0.1, 0.5, 1.0, 1.5, 2.0, 2.5]) {
  try {
    const resp = await client.chat.completions.create({
      model: "swe-1-6-slow",
      messages: ASK,
      temperature: t,
      max_tokens: 32,
    });
    const text = String(resp.choices[0].message.content ?? "").replace(/\s+/g, " ").slice(0, 24);
    console.log(`temperature=${t}  OK        text=${JSON.stringify(text)}`);
  } catch (err) {
    const msg = String(err?.message ?? "").replace(/\s+/g, " ").slice(0, 90);
    console.log(`temperature=${t}  FAILED    status=${err?.status} ${msg}`);
  }
}

// top_p has the same question: 0 is a plausible client value and may be rejected.
console.log();
for (const p of [0, 0.001, 1.0]) {
  try {
    const resp = await client.chat.completions.create({
      model: "swe-1-6-slow",
      messages: ASK,
      top_p: p,
      max_tokens: 32,
    });
    console.log(`top_p=${p}  OK        text=${JSON.stringify(String(resp.choices[0].message.content ?? "").slice(0, 24))}`);
  } catch (err) {
    console.log(`top_p=${p}  FAILED    status=${err?.status} ${String(err?.message ?? "").replace(/\s+/g, " ").slice(0, 90)}`);
  }
}
