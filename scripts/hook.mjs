#!/usr/bin/env node
// Cross-platform hook launcher for Linux / macOS / Windows (works under both sh -c and cmd /c).
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const pluginRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const binDir = path.join(pluginRoot, "bin");
const ext = process.platform === "win32" ? ".exe" : "";
const goOS = process.platform === "win32" ? "windows" : process.platform;
const goArch = process.arch === "x64" ? "amd64" : process.arch;

const candidates = [
  path.join(binDir, `azero-${goOS}-${goArch}${ext}`),
  path.join(binDir, `azero${ext}`),
  `azero${ext}`,
];

let chosen = "";
for (const c of candidates) {
  if (c.includes(path.sep)) {
    try {
      if (fs.statSync(c).isFile()) {
        chosen = c;
        break;
      }
    } catch {
      // continue
    }
  } else {
    chosen = c;
    break;
  }
}

if (!chosen) {
  process.stdout.write("{}\n");
  process.exit(0);
}

try {
  const stdinBuf = fs.readFileSync(0);
  const res = spawnSync(chosen, ["hook", ...process.argv.slice(2)], {
    input: stdinBuf,
    stdio: ["pipe", "pipe", "ignore"],
    timeout: 4500,
    windowsHide: true,
  });
  if (!res.error && res.status === 0 && res.stdout && res.stdout.length > 0) {
    process.stdout.write(res.stdout);
  } else {
    process.stdout.write("{}\n");
  }
} catch {
  process.stdout.write("{}\n");
}
