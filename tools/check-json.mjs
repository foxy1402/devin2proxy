// Validates that every JSON file in the project parses, which is the only thing
// that can go wrong with the checked-in examples.
import { readFileSync } from "node:fs";

const files = process.argv.slice(2);
if (files.length === 0) {
  console.error("usage: node tools/check-json.mjs <file.json> [...]");
  process.exit(2);
}

let bad = 0;
for (const file of files) {
  try {
    JSON.parse(readFileSync(file, "utf8"));
    console.log(`ok    ${file}`);
  } catch (err) {
    console.log(`BAD   ${file}: ${err.message}`);
    bad += 1;
  }
}
process.exit(bad === 0 ? 0 : 1);
