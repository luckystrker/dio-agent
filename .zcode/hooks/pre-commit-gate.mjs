#!/usr/bin/env node
/**
 * Quality gate before `git commit` (PreToolUse hook, matcher: Bash).
 * Generic and stack-probing: safe to copy into any project.
 *
 * - Reads the hook payload from stdin; only acts when the Bash command is a `git commit`.
 * - If package.json exists: runs `npm run --if-present lint|typecheck|test`.
 * - No package.json / no scripts: passes silently (nothing to gate yet).
 * - Exit 0 = pass, 2 = block the commit, other non-zero = hook error.
 */
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const repoDir = dirname(dirname(dirname(fileURLToPath(import.meta.url))));

function readStdin() {
  try {
    return readFileSync(0, "utf8");
  } catch {
    return "";
  }
}

function isGitCommit(raw) {
  if (!raw) return false;
  let payload;
  try {
    payload = JSON.parse(raw);
  } catch {
    return false; // unreadable payload: never block
  }
  const command = payload?.tool_input?.command ?? payload?.input?.command ?? "";
  if (typeof command !== "string") return false;
  // `git ... commit`, including `git -C path commit`; not `commit --help` docs lookups etc.
  return /\bgit\b[^\n|;&]*\bcommit\b/.test(command) && !/--help/.test(command);
}

function run(script) {
  const res = spawnSync("npm", ["run", "--if-present", script], {
    cwd: repoDir,
    encoding: "utf8",
    shell: process.platform === "win32",
  });
  return { script, ok: res.status === 0, output: (res.stdout || "") + (res.stderr || "") };
}

function main() {
  if (!isGitCommit(readStdin())) process.exit(0);

  const pkgPath = join(repoDir, "package.json");
  if (!existsSync(pkgPath)) process.exit(0); // no toolchain yet: nothing to gate

  let scripts = {};
  try {
    scripts = JSON.parse(readFileSync(pkgPath, "utf8")).scripts ?? {};
  } catch {
    console.error("pre-commit-gate: package.json is not valid JSON; refusing to gate blind.");
    process.exit(2);
  }

  const gates = ["lint", "typecheck", "test"].filter((s) => scripts[s]);
  if (gates.length === 0) process.exit(0);

  const failed = [];
  for (const g of gates) {
    const r = run(g);
    if (!r.ok) {
      failed.push(r.script);
      console.error(`\n[pre-commit-gate] \`npm run ${r.script}\` failed:\n${r.output.slice(-4000)}`);
    }
  }
  if (failed.length > 0) {
    console.error(`\n[pre-commit-gate] Commit blocked: failed gate(s): ${failed.join(", ")}.`);
    process.exit(2);
  }
  process.exit(0);
}

main();
