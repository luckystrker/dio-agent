#!/usr/bin/env node
/**
 * Quality gate before `git commit` (PreToolUse hook, matcher: Bash).
 * Go edition (этап 9.1): package.json больше не источник гейтов.
 *
 * - Reads the hook payload from stdin; only acts when the Bash command is a `git commit`.
 * - If go.mod exists: runs the Go gates in order:
 *     1. `gofmt -l .`      — пустой вывод;
 *     2. `go vet ./...`    — без замечаний;
 *     3. `go test ./...`   — юнит-тесты БЕЗ живой БД (БД-тесты — под тегом db,
 *       отдельным прогоном против Postgres из docker-compose, вне коммит-гейта).
 * - No go.mod: passes silently (nothing to gate yet).
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

/** Запуск гейта; stdout+stderr вместе — gofmt пишет туда, куда удобнее. */
function runGate(cmd, args) {
  const res = spawnSync(cmd, args, {
    cwd: repoDir,
    encoding: "utf8",
    shell: process.platform === "win32",
  });
  return {
    ok: res.status === 0,
    output: ((res.stdout || "") + (res.stderr || "")).trim(),
  };
}

/** gofmt -l . : «плохие» файлы построчно. */
function unformattedGoFiles() {
  const { ok, output } = runGate("gofmt", ["-l", "."]);
  if (!ok && output === "") {
    return { error: "gofmt не запустился (нет go? установлен ли Go?)", files: [] };
  }
  const files = output
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line !== "");
  return { error: null, files };
}

function main() {
  if (!isGitCommit(readStdin())) process.exit(0);

  if (!existsSync(join(repoDir, "go.mod"))) process.exit(0); // no toolchain yet: nothing to gate

  const failed = [];

  const fmt = unformattedGoFiles();
  if (fmt.error) {
    failed.push("gofmt");
    console.error(`\n[pre-commit-gate] gofmt: ${fmt.error}`);
  } else if (fmt.files.length > 0) {
    failed.push("gofmt");
    console.error(
      `\n[pre-commit-gate] gofmt -l . нашёл неформатированные файлы:\n${fmt.files.join("\n")}\nПрогони: gofmt -w <файлы>`,
    );
  }

  const vet = runGate("go", ["vet", "./..."]);
  if (!vet.ok) {
    failed.push("go vet");
    console.error(`\n[pre-commit-gate] \`go vet ./...\` failed:\n${vet.output.slice(-4000)}`);
  }

  const test = runGate("go", ["test", "./..."]);
  if (!test.ok) {
    failed.push("go test");
    console.error(`\n[pre-commit-gate] \`go test ./...\` failed:\n${test.output.slice(-4000)}`);
  }

  if (failed.length > 0) {
    console.error(`\n[pre-commit-gate] Commit blocked: failed gate(s): ${failed.join(", ")}.`);
    process.exit(2);
  }
  process.exit(0);
}

main();
