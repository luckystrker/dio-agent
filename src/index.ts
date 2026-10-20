import { loadConfig } from "./runtime/config.js";
import { createLogger } from "./runtime/logger.js";
import { createDatabasePool } from "./runtime/db.js";
import { acquireSingleton, SingletonTakenError, type Singleton } from "./runtime/singleton.js";
import { WorkQueueRepo } from "./runtime/work-queue.js";
import { CognitiveWorker, type WorkHandler } from "./runtime/worker.js";
import { BackgroundScheduler } from "./runtime/scheduler.js";
import { withTransaction } from "./runtime/transactions.js";
import { applyDeltas } from "./runtime/deltas.js";
import { OpenAICompatModelAdapter, type CharacterModel, type ModelTurnContext } from "./character/model-adapter.js";

// Startup по §5.3: подключить БД → singleton → конфигурация → восстановить
// незавершённую работу → запустить worker, scheduler. Recovery обязан
// работать и после SIGKILL: состояние берётся из базы, не из памяти.

const config = loadConfig();
const logger = createLogger(config.logLevel);
const startedAt = Date.now();

logger.info("dio-agent запускается", {
  nodeEnv: config.nodeEnv,
  logLevel: config.logLevel,
  modelConfigured: config.model !== null,
});

const pool = createDatabasePool(config.databaseUrl);

let singleton: Singleton;
try {
  singleton = await acquireSingleton(pool);
} catch (error) {
  if (error instanceof SingletonTakenError) {
    logger.error("другой экземпляр уже работает с этой базой — завершаю запуск");
    await pool.end();
    process.exit(3);
  }
  throw error;
}
logger.info("singleton захвачен");

const repo = new WorkQueueRepo(pool);
const recovery = await repo.recoverStale();
logger.info("recovery завершён", recovery);

const model: CharacterModel | null = config.model
  ? new OpenAICompatModelAdapter({
      endpoint: config.model.endpoint,
      apiKey: config.model.apiKey,
      model: config.model.model,
      timeoutMs: config.model.timeoutMs,
      maxRetries: config.model.maxRetries,
      maxOutputTokens: config.model.maxOutputTokens,
      onUsage: (usage) =>
        logger.debug("расход модели", {
          promptTokens: usage.promptTokens,
          completionTokens: usage.completionTokens,
        }),
    })
  : null;

function stubContextFromPayload(payload: unknown): ModelTurnContext {
  // Заглушка этапа 1: сборка контекста хода — обязанность этапа 4 (§7).
  const record = (payload ?? {}) as { messages?: Array<{ role?: string; content?: string }> };
  const messages = Array.isArray(record["messages"])
    ? record["messages"]
        .filter((message) => typeof message?.["content"] === "string")
        .map((message) => ({
          role: message["role"] === "assistant" ? ("assistant" as const) : ("user" as const),
          content: String(message["content"]),
        }))
    : [];
  return {
    system: "Ты — Dio, живой персонаж в Telegram. Отвечай кратко, живо и по-русски.",
    messages,
  };
}

const handlers: Record<string, WorkHandler> = {
  consolidation: async (item) => {
    // Этап 1: механический такт уборки. Реальная консолидация памяти — этап 3.
    await withTransaction(pool, (client) =>
      applyDeltas(client, item.id, [
        { type: "append_note", text: `maintenance tick ${new Date().toISOString()}` },
      ]),
    );
  },
  direct: async (item, signal) => {
    if (!model) {
      await repo.skip(item.id, "модель не настроена (MODEL_ENDPOINT/API_KEY/MODEL)");
      return;
    }
    const result = await model.runTurn({
      turnId: item.id,
      context: stubContextFromPayload(item.payload),
      signal,
    });
    await withTransaction(pool, async (client) => {
      await applyDeltas(
        client,
        item.id,
        result.notes.map((text) => ({ type: "append_note" as const, text })),
      );
    });
    logger.info("ход выполнен", {
      id: item.id,
      decision: result.decision,
      messages: result.messages.length,
      reason: result.reason,
    });
  },
};

const worker = new CognitiveWorker(repo, handlers, logger);
const scheduler = new BackgroundScheduler(
  (kind) => repo.enqueue(kind, {}).then(() => undefined),
  logger,
);
worker.start();
scheduler.start();

const HEARTBEAT_MS = 15_000;
const heartbeat = setInterval(() => {
  logger.info("жив", { uptimeSec: Math.round((Date.now() - startedAt) / 1000) });
}, HEARTBEAT_MS);

let shuttingDown = false;
async function shutdown(reason: string, exitCode = 0): Promise<void> {
  if (shuttingDown) return;
  shuttingDown = true;
  logger.info(`останавливаюсь: ${reason}`, {
    uptimeSec: Math.round((Date.now() - startedAt) / 1000),
  });
  clearInterval(heartbeat);
  scheduler.stop();
  await worker.stop(); // ограниченно ждём текущую работу (граница — таймаут обработчика)
  try {
    await singleton.release();
  } finally {
    await pool.end();
    process.exit(exitCode);
  }
}

singleton.onLost((reason) => void shutdown(`потеряно singleton-соединение: ${reason}`, 4));
process.on("SIGINT", () => void shutdown("SIGINT"));
process.on("SIGTERM", () => void shutdown("SIGTERM"));

logger.info("запуск завершён; worker и планировщик работают");
