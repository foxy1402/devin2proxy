// A minimal tunnel proxy that speaks either SOCKS5 or HTTP CONNECT, used to
// prove the proxy's egress paths against the real backend.
//
// It implements only what devin2proxy uses: the greeting (and username/password
// when asked for) or the CONNECT request, then a blind byte pipe. That is enough
// to carry a TLS session to server.codeium.com, which is the case worth testing —
// the Go tests cover the handshakes against fakes over plain HTTP, and this
// covers a real request over a real tunnel.
//
// Usage:
//   node tools/tunnel-probe.mjs                          # socks5, 127.0.0.1:1080
//   PROXY_KIND=http PROXY_PORT=3128 node tools/tunnel-probe.mjs
//   PROXY_USER=u PROXY_PASS=p PROXY_PORT=1081 node tools/tunnel-probe.mjs
//
// then point the proxy at it, e.g.
//   set DEVIN2PROXY_PROXIES=socks5://127.0.0.1:1080
//   set DEVIN2PROXY_PROXIES=socks5://u:p@127.0.0.1:1081
//   set DEVIN2PROXY_PROXIES=http://127.0.0.1:3128
import { createServer, connect } from "node:net";

const KIND = (process.env.PROXY_KIND ?? "socks5").toLowerCase();
const PORT = Number(process.env.PROXY_PORT ?? (KIND === "http" ? 3128 : 1080));
const HOST = process.env.PROXY_HOST ?? "127.0.0.1";
const USER = process.env.PROXY_USER ?? "";
const PASS = process.env.PROXY_PASS ?? "";

if (KIND !== "socks5" && KIND !== "http") {
  console.error(`PROXY_KIND must be socks5 or http, got ${KIND}`);
  process.exit(2);
}

let seq = 0;

// attachTunnel wires the client to a fresh connection to the target once the
// handshake is done, forwarding anything already buffered past it.
function attachTunnel(client, host, port, rest, id) {
  const upstream = connect(port, host, () => {
    console.log(`[${id}] tunnel ${host}:${port}`);
    if (KIND === "socks5") client.write(Buffer.from([0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0]));
    else client.write("HTTP/1.1 200 Connection established\r\n\r\n");
    client.removeAllListeners("data");
    if (rest.length) upstream.write(rest);
    client.pipe(upstream);
    upstream.pipe(client);
  });
  upstream.on("error", (err) => {
    console.log(`[${id}] tunnel ${host}:${port} failed: ${err.code ?? err.message}`);
    if (KIND === "socks5") {
      const rep = err.code === "ECONNREFUSED" ? 0x05 : 0x03;
      client.end(Buffer.from([0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0]));
    } else {
      client.end("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n");
    }
  });
  upstream.on("close", () => client.destroy());
}

function serveSOCKS5(client, id) {
  let stage = "greeting";
  let buf = Buffer.alloc(0);
  client.on("data", (chunk) => {
    if (stage === "tunnel") return;
    buf = Buffer.concat([buf, chunk]);

    if (stage === "greeting") {
      if (buf.length < 2) return;
      const n = buf[1];
      if (buf.length < 2 + n) return;
      const methods = buf.subarray(2, 2 + n);
      buf = buf.subarray(2 + n);
      if (USER) {
        if (!methods.includes(0x02)) {
          console.log(`[${id}] no username/password method offered`);
          client.end(Buffer.from([0x05, 0xff]));
          return;
        }
        client.write(Buffer.from([0x05, 0x02]));
        stage = "auth";
      } else {
        client.write(Buffer.from([0x05, 0x00]));
        stage = "request";
      }
      return;
    }

    if (stage === "auth") {
      if (buf.length < 2) return;
      const uLen = buf[1];
      if (buf.length < 3 + uLen) return;
      const pLen = buf[2 + uLen];
      if (buf.length < 3 + uLen + pLen) return;
      const user = buf.subarray(2, 2 + uLen).toString();
      const pass = buf.subarray(3 + uLen, 3 + uLen + pLen).toString();
      buf = buf.subarray(3 + uLen + pLen);
      if (user !== USER || pass !== PASS) {
        console.log(`[${id}] rejected credentials for ${user}`);
        client.end(Buffer.from([0x01, 0x01]));
        return;
      }
      client.write(Buffer.from([0x01, 0x00]));
      stage = "request";
      return;
    }

    if (stage === "request") {
      if (buf.length < 4) return;
      if (buf[0] !== 0x05 || buf[1] !== 0x01) {
        console.log(`[${id}] unsupported request ${buf[0]}/${buf[1]}`);
        client.end(Buffer.from([0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0]));
        return;
      }
      const atyp = buf[3];
      let host;
      let off = 4;
      if (atyp === 0x01) {
        if (buf.length < off + 6) return;
        host = Array.from(buf.subarray(off, off + 4)).join(".");
        off += 4;
      } else if (atyp === 0x04) {
        if (buf.length < off + 18) return;
        const parts = [];
        for (let i = 0; i < 16; i += 2) parts.push(buf.readUInt16BE(off + i).toString(16));
        host = parts.join(":");
        off += 16;
      } else if (atyp === 0x03) {
        const len = buf[off];
        if (buf.length < off + 3 + len) return;
        host = buf.subarray(off + 1, off + 1 + len).toString();
        off += 1 + len;
      } else {
        console.log(`[${id}] unsupported address type 0x${atyp.toString(16)}`);
        client.end(Buffer.from([0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0]));
        return;
      }
      const port = buf.readUInt16BE(off);
      off += 2;
      stage = "tunnel";
      attachTunnel(client, host, port, buf.subarray(off), id);
      return;
    }
  });
}

function serveHTTP(client, id) {
  let buf = Buffer.alloc(0);
  let done = false;
  client.on("data", (chunk) => {
    if (done) return;
    buf = Buffer.concat([buf, chunk]);
    const end = buf.indexOf("\r\n\r\n");
    if (end < 0) return;

    const head = buf.subarray(0, end).toString();
    const rest = buf.subarray(end + 4);
    const [requestLine] = head.split("\r\n");
    const [method, target] = requestLine.split(" ");
    if (method !== "CONNECT") {
      console.log(`[${id}] ${method} is not supported, only CONNECT`);
      client.end("HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n");
      return;
    }
    done = true;

    if (USER) {
      const want = "Basic " + Buffer.from(`${USER}:${PASS}`).toString("base64");
      const got = /^proxy-authorization:\s*(.+)$/im.exec(head)?.[1]?.trim();
      if (got !== want) {
        console.log(`[${id}] CONNECT ${target} rejected: bad or missing Proxy-Authorization`);
        client.end("HTTP/1.1 407 Proxy Authentication Required\r\n" +
          'Proxy-Authenticate: Basic realm="probe"\r\nContent-Length: 0\r\n\r\n');
        return;
      }
    }

    const [host, port] = target.split(":");
    attachTunnel(client, host, Number(port), rest, id);
  });
}

const server = createServer((client) => {
  const id = ++seq;
  client.on("error", () => client.destroy());
  if (KIND === "socks5") serveSOCKS5(client, id);
  else serveHTTP(client, id);
});

server.listen(PORT, HOST, () => {
  console.log(`${KIND} tunnel probe listening on ${HOST}:${PORT}` +
    (USER ? " (username/password required)" : " (no auth)"));
});

for (const sig of ["SIGINT", "SIGTERM"]) {
  process.on(sig, () => {
    console.log(`\n${seq} connection(s) handled`);
    process.exit(0);
  });
}
