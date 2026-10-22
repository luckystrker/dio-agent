// Спецификационные тесты приёмки этапа 6 (цикл приёмки, роль — тестировщик).
// Свойства формулируются ПО СПЕЦИФИКАЦИИ, а не по реализации:
//   §20 — «Каждый ход логирует IDs, action, основания существенных изменений,
//          latency, usage, budget reservation и outcome. Полный private prompt
//          по умолчанию не логируется»; метрики «queue wait и возраст старейшего
//          turn»; терминальные пропуски видны (чек-лист 6.5).
//   §16/§17 — usage приходит из ответа провайдера (usage.prompt_tokens /
//          completion_tokens) и привязывается к конкретному вызову модели;
//          резерв бюджета — до платной операции.
// Красный тест = находка несоответствия спецификации. Тесты не подгоняются под код.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Pool } from "pg";
import { ingestUpdate } from "../telegram/ingest.js";
import { ChatBufferManager } from "../telegram/buffers.js";
import { OutboxRepo } from "../telegram/outbox.js";
import { OpenAICompatModelAdapter, type ModelTransport } from "../character/model-adapter.js";
import { makeTurnHandler } from "../character/turn-handler.js";
import { makeIntentionHandler } from "../background/intention-handler.js";
import { IntentionRepo } from "../background/intentions.js";
import { InitiativeRepo } from "../character/initiative.js";
import { PersonaRepo } from "../character/persona.js";
import { MoodRepo } from "../character/mood.js";
import { RelationshipRepo } from "../character/relationships.js";
import { MemoryRepo } from "../memory/memory-repo.js";
import { BudgetRepo } from "../runtime/budgets.js";
import { WorkQueueRepo } from "../runtime/work-queue.js";
import { CognitiveWorker } from "../runtime/worker.js";
import type { Logger } from "../runtime/logger.js";
import { cleanMemoryTables, cleanTables, createTestPool, sleep } from "../test-support/db.js";
import { cleanTelegramTables, waitForDb } from "../test-support/telegram-db.js";

// ---------------------------------------------------------------------------
// Общие помощники
// ---------------------------------------------------------------------------

const CHAT_ID = "515117";
const ACTOR_ID = 4242;

/** Отличимые маркеры: проверяем вхождения конкретных значений, а не «хоть чего-то». */
const MOOD_REASON = "основание: мрачная статистика праздничных аварий §20";
const USER_TEXT =
  "Приватная проверка промпта: слушай, тут жуткая статистика аварий в новогодние праздники, маркер Alpha-Bravo-9177.";
const USAGE_PROMPT = 1234;
const USAGE_COMPLETION = 777;

interface LogEntry {
  level: string;
  message: string;
  extra?: unknown;
}

function makeRecordingLogger(): { logger: Logger; entries: LogEntry[] } {
  const entries: LogEntry[] = [];
  const logger: Logger = {
    debug: (message, extra) => entries.push({ level: "debug", message, extra }),
    info: (message, extra) => entries.push({ level: "info", message, extra }),
    warn: (message, extra) => entries.push({ level: "warn", message, extra }),
    error: (message, extra) => entries.push({ level: "error", message, extra }),
  };
  return { logger, entries };
}

/** Безопасная сериализация записи журнала (BigInt, undefined, циклы). */
function serializeEntry(entry: LogEntry): string {
  try {
    return (
      JSON.stringify(entry, (_key, value) =>
        typeof value === "bigint" ? value.toString() : value,
      ) ?? ""
    );
  } catch {
    return `${entry.level} ${entry.message} ${String(entry.extra)}`;
  }
}

/** Глубокий поиск числового поля с ключом вида latency или latencyMs (§20 «latency»). */
function findLatencyNumber(value: unknown, seen: WeakSet<object> = new WeakSet()): number | undefined {
  if (value === null || typeof value !== "object") return undefined;
  if (seen.has(value)) return undefined;
  seen.add(value);
  if (Array.isArray(value)) {
    for (const item of value) {
      const found = findLatencyNumber(item, seen);
      if (found !== undefined) return found;
    }
    return undefined;
  }
  for (const [key, inner] of Object.entries(value as Record<string, unknown>)) {
    if (/latency/i.test(key) && typeof inner === "number" && Number.isFinite(inner)) {
      return inner;
    }
    const found = findLatencyNumber(inner, seen);
    if (found !== undefined) return found;
  }
  return undefined;
}

/** Latency записи: числовое поле с ключом latency в extra либо «latency…N» в тексте. */
function latencyOf(entry: LogEntry): number | undefined {
  const fromExtra = findLatencyNumber(entry.extra);
  if (fromExtra !== undefined) return fromExtra;
  const match = /latency[^0-9]{0,24}([0-9]+)/i.exec(entry.message);
  return match ? Number(match[1]) : undefined;
}

interface ModelScriptStep {
  result: Record<string, unknown>;
  usage: { prompt_tokens: number; completion_tokens: number };
}

interface CapturedRequest {
  system: string;
  messages: Array<{ role: string; content: string }>;
}

/** Модель-заглушка: сценарные ответы с usage провайдера, запросы записываются. */
function makeScriptedModel(script: ModelScriptStep[], captured: CapturedRequest[]) {
  let next = 0;
  const transport: ModelTransport = async (_url, init) => {
    const body = JSON.parse(init.body) as { messages: CapturedRequest["messages"] };
    captured.push({
      system: body.messages.find((message) => message.role === "system")?.content ?? "",
      messages: body.messages,
    });
    const step = script[next];
    next += 1;
    if (!step) throw new Error("сценарий модели исчерпан: непредвиденный вызов LLM");
    return new Response(
      JSON.stringify({
        choices: [
          {
            message: {
              tool_calls: [
                {
                  type: "function",
                  function: { name: "turn_result", arguments: JSON.stringify(step.result) },
                },
              ],
            },
          },
        ],
        usage: step.usage,
      }),
    );
  };
  return new OpenAICompatModelAdapter({
    endpoint: "http://stub",
    apiKey: "k",
    model: "stub",
    transport,
  });
}

function rawUpdate(updateId: number, text: string): unknown {
  return {
    update_id: updateId,
    message: {
      message_id: updateId,
      from: { id: ACTOR_ID, is_bot: false },
      chat: { id: Number(CHAT_ID), type: "private" },
      date: Math.floor(Date.now() / 1000),
      text,
    },
  };
}

async function cleanStage6Tables(pool: Pool): Promise<void> {
  await cleanTelegramTables(pool);
  await cleanMemoryTables(pool);
  await pool.query("DELETE FROM mood_state");
  await pool.query("TRUNCATE TABLE proactive_log, intentions RESTART IDENTITY CASCADE");
  await pool.query("TRUNCATE TABLE personas RESTART IDENTITY CASCADE");
}

/** Сдвиг последовательностей: ID хода/работы становятся отличимыми (831002, 472002…),
 *  чтобы проверка «ID есть в журнале» не совпадала с любой цифрой случайно. */
async function bumpSequences(pool: Pool): Promise<void> {
  await pool.query("SELECT setval('work_items_id_seq', 831001, true)");
  await pool.query("SELECT setval('turns_id_seq', 472001, true)");
}

interface WorkRow {
  id: string;
  status: string;
  skip_reason: string | null;
}

interface PipelineResult {
  entries: LogEntry[];
  capturedRequests: CapturedRequest[];
  turns: Array<{ id: string; status: string }>;
  workItems: WorkRow[];
  budgetOperations: string[];
}

/** Живой прогон одного хода: ingest → буфер → worker → модель-заглушка → применение. */
async function runTurnPipeline(options: {
  script: ModelScriptStep[];
  modelDailyCallsLimit: number;
  /** Исчерпать бюджет заранее: строка резерва на сегодня уже есть. */
  preseedBudget?: boolean;
}): Promise<PipelineResult> {
  const pool = createTestPool();
  const { logger, entries } = makeRecordingLogger();
  const capturedRequests: CapturedRequest[] = [];
  let worker: CognitiveWorker | undefined;
  try {
    await cleanStage6Tables(pool);
    await bumpSequences(pool);
    if (options.preseedBudget) {
      await pool.query(
        "INSERT INTO budget_reservations (operation_id, day) VALUES ($1, CURRENT_DATE)",
        ["model:stage6-preseed"],
      );
    }

    const model = makeScriptedModel(options.script, capturedRequests);
    const persona = await new PersonaRepo(pool).active();
    const memory = new MemoryRepo(pool);
    const mood = new MoodRepo(pool);
    const relationships = new RelationshipRepo(pool);
    const modelBudget = new BudgetRepo(pool);
    const intentionsRepo = new IntentionRepo(pool);
    const initiativeRepo = new InitiativeRepo(pool);
    const outbox = new OutboxRepo(pool);
    const workRepo = new WorkQueueRepo(pool);

    const handleTurn = makeTurnHandler({
      pool,
      model,
      outbox,
      memory,
      intentions: intentionsRepo,
      persona,
      mood,
      relationships,
      workQueue: workRepo,
      modelBudget,
      modelDailyCallsLimit: options.modelDailyCallsLimit,
      historyMessageLimit: 1000,
      historyMaxChars: 200_000,
      logger,
    });
    const handleIntention = makeIntentionHandler({
      pool,
      model,
      mood,
      memory,
      persona,
      outbox,
      intentions: intentionsRepo,
      initiative: initiativeRepo,
      workQueue: workRepo,
      modelBudget,
      modelDailyCallsLimit: options.modelDailyCallsLimit,
      logger,
    });

    const buffers = new ChatBufferManager({ pool, logger });
    worker = new CognitiveWorker(
      workRepo,
      { direct: handleTurn, group: handleTurn, intention: handleIntention },
      logger,
      { pollIntervalMs: 50 },
    );
    worker.start();

    const ingested = await ingestUpdate(pool, rawUpdate(90_001, USER_TEXT));
    expect(ingested.inserted).toBe(true);
    buffers.add({
      eventId: ingested.eventId!,
      chatId: CHAT_ID,
      chatType: "private",
      kind: "message",
      text: USER_TEXT,
      occurredAt: new Date(),
    });

    await waitForDb(
      async () => {
        const row = await pool.query<{ done: string; skipped: string }>(
          "SELECT count(*) FILTER (WHERE status = 'done')::text AS done, " +
            "count(*) FILTER (WHERE status = 'skipped')::text AS skipped FROM work_items",
        );
        return Number(row.rows[0]!.done) + Number(row.rows[0]!.skipped) >= 1;
      },
      "работа хода достигла терминального статуса (done/skipped)",
      20_000,
    );
    // Небольшая пауза: финальные записи журнала должны лечь после смены статуса.
    await sleep(200);

    const turns = await pool.query<{ id: string; status: string }>(
      "SELECT id::text AS id, status FROM turns",
    );
    const workItems = await pool.query<WorkRow>(
      "SELECT id::text AS id, status, skip_reason FROM work_items",
    );
    const budgetOperations = await pool.query<{ operation_id: string }>(
      "SELECT operation_id FROM budget_reservations WHERE operation_id LIKE 'model:%'",
    );
    return {
      entries,
      capturedRequests,
      turns: turns.rows,
      workItems: workItems.rows,
      budgetOperations: budgetOperations.rows.map((row) => row.operation_id),
    };
  } finally {
    if (worker) await worker.stop();
    await pool.end();
  }
}

const ANSWER_SCRIPT: ModelScriptStep[] = [
  {
    result: {
      decision: "answer",
      messages: ["Мда, мрачная тема."],
      notes: ["тема: аварийность в праздники"],
      mood_delta: { valence: -0.2, reason: MOOD_REASON },
    },
    usage: { prompt_tokens: USAGE_PROMPT, completion_tokens: USAGE_COMPLETION },
  },
];

// ---------------------------------------------------------------------------
// §20: журнал хода
// ---------------------------------------------------------------------------

describe("§20: журнал хода — каждый ход логирует IDs, action, основания, latency, usage, budget и outcome", () => {
  it(
    "выполненный ход оставляет запись журнала, где одновременно присутствуют все обязательные измерения хода",
    { timeout: 60_000 },
    async () => {
      const run = await runTurnPipeline({ script: ANSWER_SCRIPT, modelDailyCallsLimit: 50 });

      // Пре-условия сценария: ход применён, работа завершена, бюджет зарезервирован.
      expect(run.turns.filter((turn) => turn.status === "applied")).toHaveLength(1);
      expect(run.workItems.filter((item) => item.status === "done")).toHaveLength(1);
      expect(run.budgetOperations).toHaveLength(1);
      const turnId = run.turns.find((turn) => turn.status === "applied")!.id;
      const workItemId = run.workItems.find((item) => item.status === "done")!.id;
      const operationId = run.budgetOperations[0]!;

      const dimensions: Record<string, (serialized: string) => boolean> = {
        "ID работы (work_items)": (s) => s.includes(workItemId),
        "ID хода (turns)": (s) => s.includes(turnId),
        "action (answer)": (s) => s.includes("answer"),
        "outcome (applied)": (s) => s.includes("applied"),
        "usage из ответа провайдера (1234/777)": (s) =>
          s.includes(String(USAGE_PROMPT)) && s.includes(String(USAGE_COMPLETION)),
        "budget reservation (operation_id)": (s) => s.includes(operationId),
        "основание существенного изменения (reason mood_delta)": (s) => s.includes(MOOD_REASON),
      };

      // Ход должен быть опознаваем в журнале по своим ID.
      const withWorkId = run.entries.filter((entry) =>
        serializeEntry(entry).includes(workItemId),
      );
      expect(
        withWorkId.length,
        "записи журнала, идентифицирующие ход по ID работы, не найдены",
      ).toBeGreaterThan(0);

      // §20: ОДНА запись хода несёт все измерения одновременно.
      const complete = run.entries.find((entry) => {
        const serialized = serializeEntry(entry);
        return (
          Object.values(dimensions).every((check) => check(serialized)) &&
          latencyOf(entry) !== undefined &&
          latencyOf(entry)! >= 0
        );
      });

      const coverage = Object.entries(dimensions)
        .map(
          ([name, check]) =>
            `${name}: ${run.entries.filter((entry) => check(serializeEntry(entry))).length} записей`,
        )
        .concat([
          `latency (числовое поле): ${run.entries.filter((entry) => latencyOf(entry) !== undefined).length} записей`,
        ])
        .join("\n");
      expect(
        complete,
        `ожидалась запись журнала хода, содержащая ВСЕ измерения §20 одновременно.\nПокрытие по всем ${run.entries.length} записям журнала:\n${coverage}`,
      ).toBeDefined();
    },
  );

  it(
    "полный private prompt (системная секция и сообщения запроса к модели) не попадает в журнал",
    { timeout: 60_000 },
    async () => {
      const run = await runTurnPipeline({ script: ANSWER_SCRIPT, modelDailyCallsLimit: 50 });

      expect(run.capturedRequests).toHaveLength(1); // ровно один модельный вызов
      const request = run.capturedRequests[0]!;
      const serializedEntries = run.entries.map(serializeEntry);

      if (request.system.length > 0) {
        const systemViolators = serializedEntries.filter((s) => s.includes(request.system));
        expect(
          systemViolators.length,
          "§20: полный приватный промпт (системная секция) не должен логироваться по умолчанию",
        ).toBe(0);
      }

      const fullMessages = JSON.stringify(request.messages);
      const messagesViolators = serializedEntries.filter((s) => s.includes(fullMessages));
      expect(
        messagesViolators.length,
        "§20: полный приватный промпт (сообщения запроса) не должен логироваться по умолчанию",
      ).toBe(0);
    },
  );

  it(
    "терминальный пропуск (исчерпание дневного бюджета) виден в журнале, с причиной, и без вызова модели",
    { timeout: 60_000 },
    async () => {
      const run = await runTurnPipeline({
        script: [], // вызова быть не должно — сценарий пуст
        modelDailyCallsLimit: 1,
        preseedBudget: true, // на сегодня уже есть 1 резерв → лимит исчерпан
      });

      const skipped = run.workItems.filter((item) => item.status === "skipped");
      expect(skipped).toHaveLength(1);
      const workItemId = skipped[0]!.id;

      // §16: резерв и проверка — до платной операции; вызова транспорта быть не должно.
      expect(
        run.capturedRequests,
        "при исчерпанном бюджете модельный вызов не должен происходить (§16)",
      ).toHaveLength(0);

      // §20/чек-лист 6.5: терминальный пропуск виден в журнале, привязан к работе.
      // Маркер исхода — «skip» или «пропуск» (журнал проекта на русском).
      const skipEntries = run.entries.filter(
        (entry) =>
          serializeEntry(entry).includes(workItemId) && /(skip|пропуск)/i.test(serializeEntry(entry)),
      );
      expect(
        skipEntries.length,
        "терминальный пропуск должен быть виден в журнале: запись с ID работы и исходом skip",
      ).toBeGreaterThan(0);
      // Уровень записи: терминальный пропуск — предупреждение эксплуатации (§20),
      // а не рядовое событие. Мутация warn→info должна ловиться тестом.
      expect(
        skipEntries.filter((entry) => entry.level === "warn").length,
        "запись о терминальном пропуске должна иметь уровень warn (§20), не info/debug",
      ).toBeGreaterThan(0);

      // §11.3: пропуск фиксируется с причиной; причина видна и в журнале.
      expect(skipped[0]!.skip_reason, "пропуск должен иметь причину (§11.3)").toBeTruthy();
      const reason = skipped[0]!.skip_reason!;
      const reasonVisible = run.entries.filter((entry) =>
        serializeEntry(entry).includes(reason),
      );
      expect(
        reasonVisible.length,
        "причина терминального пропуска должна быть видна в журнале (§20 outcome, §11.3)",
      ).toBeGreaterThan(0);
    },
  );
});

// ---------------------------------------------------------------------------
// §20: наблюдаемость очереди (queueStats)
// ---------------------------------------------------------------------------

describe("§20: queueStats — глубина очереди и возраст старейшего ожидающего turn", () => {
  let pool: Pool;
  let repo: WorkQueueRepo;

  beforeEach(async () => {
    pool = createTestPool();
    repo = new WorkQueueRepo(pool);
    await cleanTables(pool);
  });

  afterEach(async () => {
    await pool.end();
  });

  it("пустая очередь: глубина 0, возраст отсутствует", async () => {
    const stats = await repo.queueStats();
    expect(stats.pending).toBe(0);
    expect(stats.oldestPendingAgeSec).toBeNull();
  });

  it("возраст считается от старейшей ОЖИДАЮЩЕЙ работы: выполненная и молодая не влияют", async () => {
    const oldest = await repo.enqueue("direct", {});
    const mid = await repo.enqueue("direct", {});
    const young = await repo.enqueue("direct", {});

    const claimed = await repo.claimNext(); // FIFO → oldest
    expect(claimed!.id).toBe(oldest);
    await repo.complete(oldest);

    // Возрасты: выполненной — 1000 с, ожидающих — 300 с и 30 с.
    await pool.query("UPDATE work_items SET created_at = now() - interval '1000 seconds' WHERE id = $1", [oldest]);
    await pool.query("UPDATE work_items SET created_at = now() - interval '300 seconds' WHERE id = $1", [mid]);
    await pool.query("UPDATE work_items SET created_at = now() - interval '30 seconds' WHERE id = $1", [young]);

    const stats = await repo.queueStats();
    expect(stats.pending).toBe(2);
    expect(stats.oldestPendingAgeSec).not.toBeNull();
    const age = stats.oldestPendingAgeSec!;
    expect(age, "возраст ≈ старейшей pending-работы (300 с)").toBeGreaterThanOrEqual(295);
    expect(
      age,
      "возраст не должен браться с выполненной работы (1000 с) или молодой (30 с)",
    ).toBeLessThanOrEqual(350);
  });

  it("возраст ожидания растёт со временем, пока работа не взята (queue wait)", async () => {
    await repo.enqueue("direct", {});
    const before = await repo.queueStats();
    await sleep(1_300);
    const after = await repo.queueStats();
    expect(before.oldestPendingAgeSec).not.toBeNull();
    expect(after.oldestPendingAgeSec).not.toBeNull();
    expect(
      after.oldestPendingAgeSec!,
      "возраст старейшей работы растёт с ожиданием",
    ).toBeGreaterThan(before.oldestPendingAgeSec!);
  });
});

// ---------------------------------------------------------------------------
// §17/§16: usage привязывается к конкретному вызову модели
// ---------------------------------------------------------------------------

describe("§17: usage конкретного вызова модели приходит из ответа провайдера", () => {
  const BASE_INPUT = {
    turnId: "turn-x",
    context: { system: "ты Dio", messages: [{ role: "user" as const, content: "привет" }] },
  };

  function usageResponse(promptTokens: number, completionTokens: number): Response {
    return new Response(
      JSON.stringify({
        choices: [
          {
            message: {
              tool_calls: [
                {
                  type: "function",
                  function: {
                    name: "turn_result",
                    arguments: JSON.stringify({ decision: "ignore" }),
                  },
                },
              ],
            },
          },
        ],
        usage: { prompt_tokens: promptTokens, completion_tokens: completionTokens },
      }),
      { status: 200 },
    );
  }

  it("два последовательных вызова: каждый onUsage получает числа своего ответа провайдера ровно один раз; колбэк адаптера не вызывается", async () => {
    const adapterOnUsage = vi.fn();
    const firstCall = vi.fn();
    const secondCall = vi.fn();
    const usages = [
      { prompt: 4101, completion: 209 },
      { prompt: 5230, completion: 68 },
    ];
    let transportCalls = 0;
    const transport: ModelTransport = async () => {
      const usage = usages[transportCalls]!;
      transportCalls += 1;
      return usageResponse(usage.prompt, usage.completion);
    };
    const adapter = new OpenAICompatModelAdapter({
      endpoint: "http://stub",
      apiKey: "k",
      model: "stub",
      transport,
      onUsage: adapterOnUsage,
    });

    await adapter.runTurn({ ...BASE_INPUT, turnId: "turn-A", onUsage: firstCall });
    await adapter.runTurn({ ...BASE_INPUT, turnId: "turn-B", onUsage: secondCall });

    expect(firstCall, "usage первого вызова приходит ровно один раз").toHaveBeenCalledTimes(1);
    expect(secondCall, "usage второго вызова приходит ровно один раз").toHaveBeenCalledTimes(1);

    const firstArg = JSON.stringify(firstCall.mock.calls[0]?.[0]);
    expect(firstArg, "usage первого вызова — числа из первого ответа провайдера").toMatch(/4101/);
    expect(firstArg).toMatch(/209/);
    expect(firstArg, "usage чужого вызова не подтекает").not.toMatch(/5230/);

    const secondArg = JSON.stringify(secondCall.mock.calls[0]?.[0]);
    expect(secondArg, "usage второго вызова — числа из второго ответа провайдера").toMatch(/5230/);
    expect(secondArg).toMatch(/68/);
    expect(secondArg).not.toMatch(/4101/);

    expect(
      adapterOnUsage,
      "колбэк адаптера не должен получать чужой usage при наличии onUsage вызова",
    ).not.toHaveBeenCalled();
  });

  it("после retry usage фиксируется один раз — по успешной попытке, с её числами", async () => {
    const onUsage = vi.fn();
    let transportCalls = 0;
    const transport: ModelTransport = async () => {
      transportCalls += 1;
      if (transportCalls === 1) return new Response("rate limited", { status: 429 });
      return usageResponse(314, 159);
    };
    const adapter = new OpenAICompatModelAdapter({
      endpoint: "http://stub",
      apiKey: "k",
      model: "stub",
      transport,
      onUsage,
      maxRetries: 2,
    });

    const result = await adapter.runTurn(BASE_INPUT);
    expect(result.decision).toBe("ignore");
    expect(transportCalls).toBe(2); // первая попытка + retry
    expect(
      onUsage,
      "usage фиксируется один раз — по факту успешной операции, а не по числу попыток",
    ).toHaveBeenCalledTimes(1);
    const arg = JSON.stringify(onUsage.mock.calls[0]?.[0]);
    expect(arg).toMatch(/314/);
    expect(arg).toMatch(/159/);
  });
});
