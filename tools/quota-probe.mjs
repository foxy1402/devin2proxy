// A stand-in backend for the quota-aware cooldown, so the path that only a real
// rate limit would trigger can be checked without spending the account's daily
// quota.
//
// It refuses every chat request with 429 and answers the pool's seat-management
// call with a recorded real response, so what the pool decodes and acts on is the
// backend's own bytes rather than something this probe invented.
//
//   node tools/quota-probe.mjs bin/status-fixture.bin
//   set WINDSURF_API_SERVER_URL=http://127.0.0.1:8792
//   set DEVIN2PROXY_TOKENS=devin-session-token$probe-a,devin-session-token$probe-b
//   devin2proxy.exe
//
// Then send one completion. Both accounts are refused, the request fails as it
// would against the real backend under a rate limit, and — the part under test —
// the pool reports the refusal to the status endpoint exactly once per account and
// holds each one out until the reset in the fixture. GET /healthz shows it:
// accounts_quota_held counts the holds, and accounts_available does not.
//
// The fixture is made with a live call, which is the only step that touches the
// network for real:
//
//   bin\devin-status.exe -save bin\status-fixture.bin
//
// Ctrl-C prints a summary of the paths that were hit, which is how the "one status
// call per refusal episode, not per request" property becomes visible.
import { createServer } from "node:http";
import { readFileSync } from "node:fs";

const fixturePath = process.argv[2];
if (!fixturePath) {
  console.error("usage: node tools/quota-probe.mjs <status-fixture.bin>");
  process.exit(2);
}
const fixture = readFileSync(fixturePath);

const port = Number(process.env.PROBE_PORT ?? 8792);
const counts = { chat: 0, status: 0, other: 0 };

const server = createServer((req, res) => {
  const path = req.url ?? "";
  let body = 0;
  req.on("data", (c) => (body += c.length));
  req.on("end", () => {
    if (path.includes("GetChatMessage")) {
      counts.chat++;
      // The status the pool treats as "ask how much quota is left".
      res.writeHead(429, { "content-type": "text/plain" });
      res.end("resource exhausted: daily quota spent\n");
      console.log(`[${counts.chat}] chat     ${path}  429  ${body} bytes in`);
      return;
    }
    if (path.includes("GetUserStatus")) {
      counts.status++;
      res.writeHead(200, { "content-type": "application/proto" });
      res.end(fixture);
      console.log(`[${counts.status}] status   ${path}  200  ${body} bytes in, ${fixture.length} out`);
      return;
    }
    counts.other++;
    res.writeHead(404, { "content-type": "text/plain" });
    res.end("no such rpc\n");
    console.log(`[?] other    ${path}  404`);
  });
});

server.listen(port, "127.0.0.1", () => {
  console.log(`quota probe on http://127.0.0.1:${port}, serving a ${fixture.length}-byte status fixture`);
  console.log("every chat request answers 429; point WINDSURF_API_SERVER_URL here");
});

process.on("SIGINT", () => {
  console.log(`\nchat ${counts.chat}, status ${counts.status}, other ${counts.other}`);
  console.log(
    counts.status > 0 && counts.status < counts.chat
      ? "status calls were fewer than chat refusals, which is the dedupe working"
      : "status calls were not fewer than chat refusals"
  );
  server.close(() => process.exit(0));
});
