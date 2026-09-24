// Scans the devin binary for the strings that would confirm or refute the image
// field mapping we inherited from third-party prior art. Output is bounded so a
// 190 MB binary does not flood the terminal.
import fs from "node:fs";

const path = process.argv[2];
const patterns = process.argv.slice(3);
const buf = fs.readFileSync(path);
console.log(`read ${(buf.length / 1e6).toFixed(0)} MB from ${path}`);

for (const pat of patterns) {
  const needle = Buffer.from(pat, "latin1");
  const hits = [];
  let from = 0;
  for (;;) {
    const i = buf.indexOf(needle, from);
    if (i < 0) break;
    hits.push(i);
    from = i + 1;
    if (hits.length > 400) break;
  }

  // Collect the printable neighbourhood of the first few hits, deduped.
  const seen = new Set();
  for (const i of hits.slice(0, 40)) {
    const start = Math.max(0, i - 60);
    const end = Math.min(buf.length, i + needle.length + 60);
    const ctx = buf
      .subarray(start, end)
      .toString("latin1")
      .replace(/[^\x20-\x7e]/g, "·");
    if (!seen.has(ctx)) seen.add(ctx);
  }
  console.log(`\n=== "${pat}": ${hits.length > 400 ? ">400" : hits.length} hit(s) ===`);
  let shown = 0;
  for (const ctx of seen) {
    if (shown++ >= 6) break;
    console.log("   " + ctx);
  }
}
