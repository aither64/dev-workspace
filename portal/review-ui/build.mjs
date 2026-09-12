import {build} from "esbuild";
import {readFile, readdir, mkdir, writeFile} from "node:fs/promises";
import path from "node:path";

await mkdir("dist", {recursive: true});
const bundled = await build({metafile: true, entryPoints: {"review-editor": "editor.js", "review-highlight-worker": "highlight-worker.js"}, outdir: "dist", bundle: true, format: "esm", target: "es2022", minify: true, legalComments: "external", charset: "utf8"});
await writeFile("dist/review-build.json", `${JSON.stringify(bundled.metafile)}\n`);
const lock = JSON.parse(await readFile("package-lock.json", "utf8"));
const notices = ["Third-party dependencies for workspace repository review.\nThe exact dependency graph and integrity hashes are in package-lock.json."];
notices.push(await readFile("syntax.NOTICE", "utf8"));
const manifest = [];
const esbuildLicense = await readFile("node_modules/esbuild/LICENSE.md", "utf8");
for (const [location, entry] of Object.entries(lock.packages).sort(([a], [b]) => a.localeCompare(b, "en"))) {
  if (!location) continue;
  const name = location.split("node_modules/").at(-1);
  manifest.push({name, version: entry.version, license: entry.license, integrity: entry.integrity});
  let license;
  try {
    const names = await readdir(location);
    const files = names.filter(name => /^(license|licence|copying)(\.|$)/i.test(name)).sort();
    if (!files.length && !name.startsWith("@esbuild/")) throw new Error(`No license file for ${name}`);
    license = files.length ? (await Promise.all(files.map(file => readFile(path.join(location, file), "utf8")))).join("\n") : esbuildLicense;
  } catch (error) {
    // Every architecture package is part of esbuild's MIT-licensed distribution.
    // npm installs only the current platform; retain its license for all locked
    // optional platform entries as well, including the build tool itself.
    if (!name.startsWith("@esbuild/") || !entry.optional || entry.license !== "MIT") throw error;
    license = esbuildLicense;
  }
  notices.push(`\n--- ${name}@${entry.version} (${entry.license}) ---\n${license.trim()}\n`);
}
await writeFile("dist/review-editor.LICENSE.txt", notices.join("\n"));
await writeFile("dist/review-editor.dependencies.json", `${JSON.stringify(manifest, null, 2)}\n`);
