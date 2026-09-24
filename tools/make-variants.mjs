// Builds credential-shape variants from the real token so we can see how the
// backend's auth layer reacts to each shape. The informative case is a JWT with a
// valid structure and a broken signature: if that produces a different error from
// a random string, the backend is parsing session tokens specifically rather than
// looking up an API key.
import fs from "node:fs";

const path = process.env.APPDATA + "\\devin\\credentials.toml";
const text = fs.readFileSync(path, "utf8");
const m = /windsurf_api_key\s*=\s*"([^"]+)"/.exec(text);
if (!m) {
  console.error("no windsurf_api_key in " + path);
  process.exit(1);
}
const token = m[1];
const dollar = token.indexOf("$");
const prefix = dollar >= 0 ? token.slice(0, dollar) : "";
const jwt = dollar >= 0 ? token.slice(dollar + 1) : token;
const parts = jwt.split(".");

console.log("prefix        :", JSON.stringify(prefix));
console.log("token length  :", token.length);
console.log("jwt segments  :", parts.length);
console.log("jwt header    :", Buffer.from(parts[0], "base64url").toString());
console.log("jwt payload   :", Buffer.from(parts[1], "base64url").toString());
console.log("signature len :", parts[2]?.length ?? 0);

// Flip one character of the signature: structure stays valid, signature does not.
const last = parts[2].slice(-1);
const flipped = parts[2].slice(0, -1) + (last === "A" ? "B" : "A");
const tampered = `${parts[0]}.${parts[1]}.${flipped}`;

const variants = {
  "tampered signature, full token": `${prefix}$${tampered}`,
  "bare jwt, no prefix": jwt,
  "tampered signature, bare jwt": tampered,
  "prefix with junk": `${prefix}$not-a-jwt`,
  "random uuid": "0037d1a3-1c1b-4d1e-9c1f-000000000000",
  "prefix only": prefix,
};

fs.writeFileSync("variants.json", JSON.stringify(variants, null, 2));
console.log("\nwrote variants.json with", Object.keys(variants).length, "variants");
