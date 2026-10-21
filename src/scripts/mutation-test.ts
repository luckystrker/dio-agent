// Мутационное тестирование критичных модулей (этап 6.2): ingest (§6.1),
// применение дельт (§12.1), outbox (§13.1). Запуск: npm run test:mutation.
//
// Раннер свой и намеренно тонкий (в духе §17 «тонкий адаптер»): каталог
// операторных мутаций применяется к исходнику, на каждого мутанта гоняются
// его «убийцы»-тесты (юнит и/или БД), затем файл восстанавливается.
// Мутант, ломающий компиляцию, НЕ идёт в зачёт убитым (честный скор):
// при падении тестов прогоняется typecheck, и такие мутанты считаются
// отдельно как compile-error. Перед началом обязательный базовый прогон:
// все наборы убийц зелёные на немутрированном коде, иначе ранер останется
// с честной ошибкой (например, БД не поднята).
//
// Переменные окружения:
//   MUTATION_MIN_SCORE     — порог убиваемости, % (по умолчанию 90)
//   MUTATION_MAX_PER_OP    — мутантов на оператор на файл (по умолчанию 6)
//   MUTATION_ONLY          — через запятую имена файлов из списка мишеней

import { spawnSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const ROOT = join(dirname(fileURLToPath(import.meta.url)), "..", "..");

interface MutationTarget {
  file: string;
  unitKillers: string[];
  dbKillers: string[];
}

const TARGETS: MutationTarget[] = [
  {
    file: "src/telegram/ingest.ts",
    unitKillers: [],
    dbKillers: ["src/telegram/ingest.db.test.ts", "src/telegram/ingest.pbt.db.test.ts"],
  },
  {
    file: "src/runtime/deltas.ts",
    unitKillers: ["src/runtime/deltas.test.ts", "src/runtime/deltas.pbt.test.ts"],
    dbKillers: ["src/runtime/transactions.db.test.ts"],
  },
  {
    file: "src/telegram/outbox.ts",
    unitKillers: [],
    dbKillers: [
      "src/telegram/outbox.db.test.ts",
      "src/telegram/delivery.db.test.ts",
      "src/telegram/outbox-delivery.pbt.db.test.ts",
    ],
  },
];

interface Operator {
  name: string;
  /** Регэксп с группой 1 — заменяемый оператор; остальное — контекст. */
  pattern: RegExp;
  replace: (operator: string) => string;
}

// `=>` не матчится (класс [<>] не включает '='). Часть дженериков (`Record<`)
// всё же попадает под boundary — такие мутанты ломают компиляцию и уходят
// в compile-error, не в зачёт; при малом MUTATION_MAX_PER_OP это сжигает
// слоты выборки — задавай лимит с запасом.
const OPERATORS: Operator[] = [
  { name: "equality", pattern: /(===|!==)/g, replace: (op) => (op === "===" ? "!==" : "===") },
  { name: "logical", pattern: /(&&|\|\|)/g, replace: (op) => (op === "&&" ? "||" : "&&") },
  {
    name: "boundary",
    pattern: /([\w)\]]\s*)([<>]=?)(?=\s*[\w(])/g,
    replace: (op) => (op === "<" ? "<=" : op === "<=" ? "<" : op === ">" ? ">=" : ">"),
  },
  { name: "increment", pattern: /([-+]) 1/g, replace: (op) => (op === "+" ? "-" : "+") },
];

interface Mutant {
  target: MutationTarget;
  index: number; // индекс совпадения оператора
  operator: string;
  from: string;
  to: string;
  line: number;
  status: "killed" | "survived" | "compile-error" | "timeout" | "equivalent";
}

/**
 * Эквивалентные мутанты: поведение кода неотличимо от оригинала — убить
 * тестом невозможно в принципе. Перечислены явно, с обоснованием, и не
 * идут в зачёт (но видны в отчёте).
 */
const EQUIVALENT_MUTANTS: Array<{
  file: string;
  line: number;
  operator: string;
  from: string;
  to: string;
  reason: string;
}> = [
  {
    // claimDue: not_before <= now() против < now(). now() в PostgreSQL —
    // время НАЧАЛА транзакции: захватывающая транзакция всегда строго позже
    // вставившей, равных_timestamp now() у двух транзакций не бывает.
    file: "src/telegram/outbox.ts",
    line: 137,
    operator: "boundary",
    from: "<=",
    to: "<",
    reason: "now() двух транзакций в PostgreSQL никогда не равен — границы не различимы",
  },
];

const MIN_SCORE = Number(process.env["MUTATION_MIN_SCORE"] ?? 90);
const MAX_PER_OP = Number(process.env["MUTATION_MAX_PER_OP"] ?? 6);
const ONLY = (process.env["MUTATION_ONLY"] ?? "").split(",").map((name) => name.trim()).filter(Boolean);
const RUN_TIMEOUT_MS = 300_000;

function sh(command: string): { code: number; timedOut: boolean; output: string } {
  const result = spawnSync(command, {
    shell: true,
    cwd: ROOT,
    encoding: "utf8",
    timeout: RUN_TIMEOUT_MS,
    stdio: ["ignore", "pipe", "pipe"],
  });
  const output = `${result.stdout ?? ""}\n${result.stderr ?? ""}`.slice(-1500);
  if (result.error && (result.error as NodeJS.ErrnoException).code === "ETIMEDOUT") {
    return { code: 124, timedOut: true, output };
  }
  if (result.signal === "SIGTERM") return { code: 124, timedOut: true, output };
  return { code: result.status ?? 1, timedOut: false, output };
}

function runVitest(killers: string[], db: boolean): boolean {
  if (killers.length === 0) return true;
  const config = db ? "vitest.db.config.ts" : "vitest.config.ts";
  const { code } = sh(`npx vitest run --config ${config} ${killers.join(" ")}`);
  return code === 0;
}

let lastTypecheckOutput = "";

function typecheckFails(): boolean {
  const result = sh("npx tsc --noEmit");
  lastTypecheckOutput = result.output;
  return result.code !== 0;
}

function lineOfOffset(source: string, offset: number): number {
  return source.slice(0, offset).split("\n").length;
}

/** Все точки мутации одного оператора в файле (смещения групп-операторов). */
function operatorMatches(source: string, operator: Operator): Array<{ offset: number; from: string }> {
  const matches: Array<{ offset: number; from: string }> = [];
  const regex = new RegExp(operator.pattern.source, operator.pattern.flags);
  let match: RegExpExecArray | null;
  while ((match = regex.exec(source)) !== null) {
    // Двухгрупповые операторы (boundary): группа 1 — контекст, оператор — группа 2.
    // Одногрупповые (equality/logical/increment): группа 1 и есть оператор,
    // смещение — начало совпадения.
    if (match.length >= 3) {
      const context = match[1] ?? "";
      matches.push({ offset: match.index + context.length, from: match[2]! });
    } else {
      matches.push({ offset: match.index, from: match[1]! });
    }
    if (match[0].length === 0) regex.lastIndex += 1;
  }
  return matches;
}

function pickSpread<T>(items: T[], limit: number): T[] {
  if (items.length <= limit) return items;
  const step = items.length / limit;
  return Array.from({ length: limit }, (_, index) => items[Math.floor(index * step)]!);
}

function buildMutants(target: MutationTarget): Mutant[] {
  const source = readFileSync(join(ROOT, target.file), "utf8");
  const mutants: Mutant[] = [];
  for (const operator of OPERATORS) {
    const matches = operatorMatches(source, operator).filter((match) => match.from.length > 0);
    for (const match of pickSpread(matches, MAX_PER_OP)) {
      mutants.push({
        target,
        index: match.offset,
        operator: operator.name,
        from: match.from,
        to: operator.replace(match.from),
        line: lineOfOffset(source, match.offset),
        status: "survived",
      });
    }
  }
  return mutants.sort((a, b) => a.index - b.index);
}

function applyMutant(source: string, mutant: Mutant): string {
  return source.slice(0, mutant.index) + mutant.to + source.slice(mutant.index + mutant.from.length);
}

async function main(): Promise<void> {
  const targets = ONLY.length > 0 ? TARGETS.filter((t) => ONLY.includes(t.file)) : TARGETS;
  if (targets.length === 0) {
    console.error(`MUTATION_ONLY: ни одна мишень не найдена (${ONLY.join(", ")})`);
    process.exit(2);
  }

  console.log("Базовый прогон: все наборы убийц обязаны быть зелёными на чистом коде");
  for (const target of targets) {
    if (target.unitKillers.length > 0 && !runVitest(target.unitKillers, false)) {
      console.error(`БАЗА КРАСНАЯ (юнит): ${target.file} — ${target.unitKillers.join(", ")}`);
      process.exit(2);
    }
    if (target.dbKillers.length > 0 && !runVitest(target.dbKillers, true)) {
      console.error(`БАЗА КРАСНАЯ (БД): ${target.file} — ${target.dbKillers.join(", ")}`);
      console.error("Проверь, что тестовая БД поднята (docker compose up -d).");
      process.exit(2);
    }
  }
  console.log("База зелёная.");

  const results: Mutant[] = [];
  for (const target of targets) {
    const original = readFileSync(join(ROOT, target.file), "utf8");
    const allMutants = buildMutants(target);
    // эквивалентные мутанты не исполняются — только отмечаются
    const equivalents = allMutants.filter((mutant) =>
      EQUIVALENT_MUTANTS.some(
        (entry) =>
          entry.file === target.file &&
          entry.line === mutant.line &&
          entry.operator === mutant.operator &&
          entry.from === mutant.from &&
          entry.to === mutant.to,
      ),
    );
    for (const mutant of equivalents) mutant.status = "equivalent";
    results.push(...equivalents);
    const mutants = allMutants.filter((mutant) => mutant.status !== "equivalent");
    for (const mutant of equivalents) {
      console.log(
        `  эквивалентный (не исполняется): ${target.file}:${mutant.line} ${mutant.operator} '${
          mutant.from
        }' → '${mutant.to}' — ${
          EQUIVALENT_MUTANTS.find(
            (entry) => entry.file === target.file && entry.line === mutant.line && entry.operator === mutant.operator,
          )?.reason
        }`,
      );
    }
    console.log(`\n=== ${target.file}: ${mutants.length} мутантов ===`);

    try {
      for (const [number, mutant] of mutants.entries()) {
        process.stdout.write(`  [${number + 1}/${mutants.length}] ${mutant.operator} строка ${mutant.line}: '${mutant.from}' → '${mutant.to}' ... `);
        writeFileSync(join(ROOT, target.file), applyMutant(original, mutant));
        const killersPass =
          runVitest(target.unitKillers, false) && runVitest(target.dbKillers, true);
        if (killersPass) {
          mutant.status = "survived";
          console.log("ВЫЖИЛ");
        } else if (typecheckFails()) {
          mutant.status = "compile-error";
          console.log("compile-error (не в зачёте)");
          console.log(lastTypecheckOutput.split("\n").filter(Boolean).slice(0, 3).join("\n"));
        } else {
          mutant.status = "killed";
          console.log("убит");
        }
        results.push(mutant);
      }
    } finally {
      writeFileSync(join(ROOT, target.file), original);
    }
    if (original !== readFileSync(join(ROOT, target.file), "utf8")) {
      console.error(`НЕ УДАЛОСЬ ВОССТАНОВИТЬ ${target.file} — проверь git diff!`);
      process.exit(2);
    }
  }

  const scored = results.filter((mutant) => mutant.status !== "compile-error" && mutant.status !== "equivalent");
  const killed = scored.filter((mutant) => mutant.status === "killed").length;
  const score = scored.length === 0 ? 100 : Math.round((killed / scored.length) * 100);
  const survived = scored.filter((mutant) => mutant.status === "survived");

  console.log("\n===== Итог мутационного тестирования =====");
  console.log(`Мутантов в зачёте: ${scored.length} (убито: ${killed}, выжило: ${survived.length})`);
  const compileErrors = results.filter((mutant) => mutant.status === "compile-error").length;
  const equivalentsTotal = results.filter((mutant) => mutant.status === "equivalent").length;
  console.log(`Compile-error вне зачёта: ${compileErrors}`);
  console.log(`Эквивалентных (вне зачёта, с обоснованием): ${equivalentsTotal}`);
  for (const mutant of survived) {
    console.log(`  ВЫЖИЛ: ${mutant.target.file}:${mutant.line} ${mutant.operator} '${mutant.from}' → '${mutant.to}'`);
  }
  console.log(`Скор убиваемости: ${score}% (порог ${MIN_SCORE}%)`);

  writeFileSync(
    join(ROOT, ".mutation-report.json"),
    JSON.stringify({ score, threshold: MIN_SCORE, results }, null, 2),
  );

  if (score < MIN_SCORE) {
    console.error("Порог не пройден: выжившие мутанты — дыры в тестах, закрой их тестами и перегони.");
    process.exit(1);
  }
  console.log("Порог пройден.");
}

// Восстановление исходников при прерывании (Ctrl-C): раннер мутирует
// ровно один файл в один момент — возвращаем все мишени из git.
process.on("SIGINT", () => {
  console.error("\nПрервано: восстанавливаю мишени из git");
  for (const target of TARGETS) {
    spawnSync(`git checkout -- ${target.file}`, { shell: true, cwd: ROOT });
  }
  process.exit(130);
});

await main();
