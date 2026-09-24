// Tests how the backend's auth layer reacts to different credential shapes, by
// running devin-call once per variant with DEVIN_TOKEN set. The interesting one is
// a structurally valid JWT with a broken signature: if the backend parses session
// tokens specifically, that produces a different error from a random string.
//
// Uses the env override, so the real credentials file is never read or modified.
//
//   cd tools && node auth-variant-test.mjs
import { execFile } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { promisify } from "node:util";

const run = promisify(execFile);
const variants = JSON.parse(fs.readFileSync("variants.json", "utf8"));
const call = path.resolve("..", "bin", "devin-call.exe");

for (const [label, token] of Object.entries(variants)) {
  let output = "";
  try {
    const res = await run(call, ["-prompt", "Reply with exactly: pong"], {
      env: { ...process.env, DEVIN_TOKEN: token },
      timeout: 120_000,
    });
    output = res.stdout;
  } catch (err) {
    // devin-call exits non-zero on a rejected request, which is the normal case here.
    output = `${err.stdout ?? ""}${err.stderr ?? ""}${err.message ?? ""}`;
  }
  const lines = output.split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
  const error = lines.find((l) => /\[error\]|error:|unauthenticated|invalid/i.test(l));
  const text = lines
    .filter((l) => /^(Pong|pong|OK|\[text\])/i.test(l))
    .slice(0, 2)
    .join(" | ");
  console.log(`${label}\n    -> ${(error ?? text ?? "no diagnostic").slice(0, 150)}`);
}
