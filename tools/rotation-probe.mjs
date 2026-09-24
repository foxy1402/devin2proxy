// A stand-in for the Devin backend that records which credential each request
// arrived with, so account rotation can be verified without spending quota.
//
// It speaks just enough Connect to look real to the proxy: an HTTP 200 with a
// single data frame carrying "ok", then a clean end-of-stream frame. Every
// request's Authorization header is appended to the sequence it prints at the
// end, which is what makes the round-robin order observable.
//
// Usage:
//   node tools/rotation-probe.mjs               # listens on 127.0.0.1:8790
//   WINDSURF_API_SERVER_URL=http://127.0.0.1:8790 devin2proxy.exe ...
//
// The proxy reads WINDSURF_API_SERVER_URL for pool entries too, so pointing the
// proxy at this listener is all it takes to keep the whole test off the network.
import { createServer } from "node:http";

const PORT = Number(process.env.PROBE_PORT ?? 8790);
const HOST = process.env.PROBE_HOST ?? "127.0.0.1";

const seen = [];
let seq = 0;

// Connect envelope: [flags:1][length:4 BE][payload].
function frame(flags, payload) {
  const out = Buffer.alloc(5 + payload.length);
  out[0] = flags;
  out.writeUInt32BE(payload.length, 1);
  payload.copy(out, 5);
  return out;
}

// Minimal GetChatMessageResponse with delta_text = "ok":
// field 3, wire type 2 → tag 0x1a.
function responseFrame(text) {
  const body = Buffer.from(text, "utf8");
  return frame(0x00, Buffer.concat([Buffer.from([0x1a, body.length]), body]));
}

const server = createServer((req, res) => {
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => {
    const auth = req.headers.authorization ?? "";
    // "Basic <token>-<token>": the credential is the whole value joined to itself
    // by a hyphen, so it is the first half of the string. Splitting on the first
    // hyphen does not work — the token itself contains them.
    const value = auth.replace(/^Basic /i, "");
    const token = value.slice(0, Math.floor(value.length / 2));
    const label = token.length > 6 ? "…" + token.slice(-6) : token || "(none)";

    seq += 1;
    seen.push(label);
    console.log(`request ${seq}: token ${label}  body ${chunks.reduce((n, c) => n + c.length, 0)} bytes`);

    const body = Buffer.concat([
      responseFrame("ok"),
      frame(0x02, Buffer.from("{}", "utf8")),
    ]);
    res.writeHead(200, {
      "Content-Type": "application/connect+proto",
      "Content-Length": body.length,
    });
    res.end(body);
  });
});

server.listen(PORT, HOST, () => {
  console.log(`rotation probe listening on http://${HOST}:${PORT}`);
});

for (const sig of ["SIGINT", "SIGTERM"]) {
  process.on(sig, () => {
    console.log(`\n${seen.length} request(s) in arrival order:`);
    console.log("  " + seen.join(" → "));
    const distinct = [...new Set(seen)];
    console.log(`  ${distinct.length} distinct account(s): ${distinct.join(", ")}`);
    process.exit(0);
  });
}
