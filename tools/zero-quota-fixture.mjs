// Rewrites one counter inside a recorded status fixture to zero, so the offline
// quota probe can show what the pool does with an account that has run out —
// without spending the real account's daily allowance to get there.
//
//   node tools/zero-quota-fixture.mjs bin/status-fixture.bin bin/status-empty.bin
//   node tools/quota-probe.mjs bin/status-empty.bin
//
// Only the chosen field's value changes: the identity, the model list, the reset
// timestamps and every other counter are the backend's own bytes, so what the pool
// decodes is a real response with one number moved to zero. The field number
// defaults to 14 (a counter that reads like "remaining"), and a third argument
// overrides the plan-block field to zero.
import { readFileSync, writeFileSync } from "node:fs";

const [, , inPath, outPath, fieldArg] = process.argv;
if (!inPath || !outPath) {
  console.error("usage: node tools/zero-quota-fixture.mjs <in.bin> <out.bin> [field]");
  process.exit(2);
}
const target = Number(fieldArg ?? 14);

// scan walks one message and returns every field with the byte range of its value.
function scan(buf) {
  const out = [];
  let i = 0;
  while (i < buf.length) {
    const tag = readVarint(buf, i);
    i = tag.next;
    const tagValue = Number(tag.value);
    const field = tagValue >>> 3;
    const wire = tagValue & 7;
    if (wire === 0) {
      const v = readVarint(buf, i);
      out.push({ field, wire, valueStart: i, valueEnd: v.next });
      i = v.next;
    } else if (wire === 2) {
      const len = readVarint(buf, i);
      const start = len.next;
      out.push({ field, wire, valueStart: start, valueEnd: start + Number(len.value), bytes: buf.subarray(start, start + Number(len.value)) });
      i = start + Number(len.value);
    } else if (wire === 5) {
      out.push({ field, wire, valueStart: i, valueEnd: i + 4 });
      i += 4;
    } else if (wire === 1) {
      out.push({ field, wire, valueStart: i, valueEnd: i + 8 });
      i += 8;
    } else {
      throw new Error(`unsupported wire type ${wire} for field ${field} at ${i}`);
    }
  }
  return out;
}

function readVarint(buf, start) {
  let value = 0n;
  let shift = 0n;
  let i = start;
  for (;;) {
    if (i >= buf.length) throw new Error("varint ran off the end");
    const b = buf[i++];
    value |= BigInt(b & 0x7f) << shift;
    if ((b & 0x80) === 0) break;
    shift += 7n;
  }
  return { value, next: i };
}

// GetUserStatusResponse wraps user_status at field 1; the plan block is field 13
// of it and the counters sit directly on that block.
const response = readFileSync(inPath);
const outer = scan(response).find((f) => f.field === 1 && f.wire === 2);
if (!outer) throw new Error(`${inPath}: no field 1 wrapper; is this a live response?`);
const plan = scan(outer.bytes).find((f) => f.field === 13 && f.wire === 2);
if (!plan) throw new Error(`${inPath}: no plan block at field 13`);

const counters = scan(plan.bytes).filter((f) => f.wire === 0);
const chosen = counters.find((f) => f.field === target);
if (!chosen) {
  throw new Error(`field ${target} is not on the plan block; found ${counters.map((c) => c.field).join(", ")}`);
}
const before = readVarint(plan.bytes, chosen.valueStart).value;
if (chosen.valueEnd - chosen.valueStart !== 1) {
  throw new Error(`field ${target} is ${chosen.valueEnd - chosen.valueStart} bytes; this tool only rewrites single-byte values`);
}

// Written in place: zero is still one byte, so no length field anywhere changes.
const patched = Buffer.from(response);
patched[outer.valueStart + plan.valueStart + chosen.valueStart] = 0;
writeFileSync(outPath, patched, { mode: 0o600 });

console.log(`${inPath} -> ${outPath}`);
console.log(`  plan field ${target}: ${before} -> 0 (${patched.length} bytes, unchanged length)`);
console.log(`  counters now: ${counters.map((c) => `${c.field}=${c.field === target ? 0 : readVarint(plan.bytes, c.valueStart).value}`).join(" ")}`);
