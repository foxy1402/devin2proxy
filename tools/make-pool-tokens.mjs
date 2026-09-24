// Prepares a token file for the multi-account end-to-end tests.
//
// By default it writes two entries: a deliberately invalid token first, then
// whatever account the Devin CLI is currently logged in as. The order matters —
// the first request takes the bad account, so a pass proves the failover worked
// rather than that the bad account was never tried.
//
// With "real-only" as the second argument it writes just the real account, which
// is what the cancel/abort checks need: a single account makes "did the account
// get cooled?" unambiguous.
//
// The file lands in bin/, which .gitignore already covers, because it holds a
// live credential in cleartext.
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { homedir } from "node:os";
import { join, dirname } from "node:path";

const credsPath = process.env.DEVIN_CREDENTIALS_PATH
  ?? join(process.env.APPDATA ?? join(homedir(), "AppData", "Roaming"), "devin", "credentials.toml");

const toml = readFileSync(credsPath, "utf8");
const match = toml.match(/windsurf_api_key\s*=\s*"([^"]+)"/);
if (!match) throw new Error(`no windsurf_api_key in ${credsPath}`);
const real = match[1];

// A well-formed but unsigned session token: it reaches the Devin token validator
// and is refused there, which is exactly the 401 path the pool is built for.
const bad = "devin-session-token$eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9."
  + "eyJzZXNzaW9uX2lkIjoid2luZHN1cmYtc2Vzc2lvbi1kZWFkYmVlZiJ9."
  + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";

const realOnly = process.argv[3] === "real-only";
const out = process.argv[2] ?? "bin/pool-tokens.txt";
mkdirSync(dirname(out), { recursive: true });
writeFileSync(out, [
  realOnly
    ? "# Single account, for checking that a cancelled request does not cool it."
    : "# End-to-end pool test: invalid account first, real account second.",
  ...(realOnly ? [] : [bad]),
  real,
  "",
].join("\n"), { mode: 0o600 });

console.log(`wrote ${out}${realOnly ? " (real account only)" : ""}`);
if (!realOnly) console.log(`  account 1: invalid (…${bad.slice(-6)})`);
console.log(`  account ${realOnly ? 1 : 2}: real    (…${real.slice(-6)})`);

