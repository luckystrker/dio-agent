import { loadConfig } from "./runtime/config.js";
import { createLogger } from "./runtime/logger.js";
import { createDatabasePool } from "./runtime/db.js";
import { acquireSingleton, SingletonTakenError, type Singleton } from "./runtime/singleton.js";
import { WorkQueueRepo } from "./runtime/work-queue.js";
import { CognitiveWorker, SkipWorkError, type WorkHandler } from "./runtime/worker.js";
import { BackgroundScheduler } from "./runtime/scheduler.js";
import { BudgetRepo } from "./runtime/budgets.js";
import { withTransaction } from "./runtime/transactions.js";
import { applyDeltas } from "./runtime/deltas.js";
import { OpenAICompatModelAdapter, type CharacterModel, type ModelTurnContext } from "./character/model-adapter.js";
import { DryRunTelegramApi, HttpTelegramApi, type TelegramApi } from "./telegram/api.js";
import { TelegramPoller } from "./telegram/polling.js";
import { ChatBufferManager } from "./telegram/buffers.js";
import { OutboxRepo } from "./telegram/outbox.js";
import { DeliveryLoop } from "./telegram/delivery.js";

// Startup по §5.3: подключить БД → singleton → конфигурация → восстановить
// незавершённую работу → запустить polling, worker, scheduler, delivery.
// Recovery обязан работать и после SIGKILL: состояние берётся из базы.

const config = loadConfig();
const logger = createLogger(config.logLevel);
const startedAt = Date.now();

logger.info("dio-agent запускается", {
  nodeEnv: config.nodeEnv,
  logLevel: config.logLevel,
  modelConfigured: config.model !== null,
  telegramMode: config.telegram === null ? "выключен" : config.telegram.dryRun ? "dry-run" : "живой",
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
const outbox = new OutboxRepo(pool);
const recovery = await repo.recoverStale();
logger.info("recovery очереди завершён", recovery);

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

const SYSTEM_PROMPT =
  "Ты — Dio, живой персонаж в Telegram. Отвечай кратко, живо и по-русски. " +
  "Сообщения пользователя идут пачкой одного собеседника — отвечай на пачку целиком.";

function stubContextFromPayload(payload: unknown): ModelTurnContext {
  const record = (payload ?? {}) as { messages?: Array<{ role?: string; content?: string }> };
  const messages = Array.isArray(record["messages"])
    ? record["messages"]
        .filter((message) => typeof message?.["content"] === "string")
        .map((message) => ({
          role: message["role"] === "assistant" ? ("assistant" as const) : ("user" as const),
          content: String(message["content"]),
        }))
    : [];
  return { system: SYSTEM_PROMPT, messages };
}

/** Ход по turn: события → контекст → модель → транзакция применения (§12.1). */
const handleTurn: WorkHandler = async (item, signal) => {
  if (!model) {
    throw new SkipWorkError("модель не настроена", "модель не настроена (MODEL_ENDPOINT/API_KEY/MODEL)");
  }
  const payload = (item.payload ?? {}) as { turnId?: string; chatId?: string };
  const turnId = typeof payload.turnId === "string" ? payload.turnId : null;

  let context: ModelTurnContext;
  if (turnId) {
    const events = await pool.query<{ text: string | null }>(
      "SELECT text FROM social_events WHERE turn_id = $1 ORDER BY id",
      [turnId],
    );
    context = {
      system: SYSTEM_PROMPT,
      messages: events.rows.map((row) => ({
        role: "user" as const,
        content: row.text ?? "[сообщение без текста]",
      })),
    };
  } else {
    context = stubContextFromPayload(item.payload);
  }

  const result = await model.runTurn({ turnId: turnId ?? item.id, context, signal });

  await withTransaction(pool, async (client) => {
    if (turnId) {
      // CAS: применить turn не более одного раза (§12.1, инвариант 4)
      const applied = await client.query(
        "UPDATE turns SET status = 'applied' WHERE id = $1 AND status = 'created' RETURNING id",
        [turnId],
      );
      if ((applied.rowCount ?? 0) === 0) {
        logger.warn("повторное применение turn отклонено", { turnId });
        return;
      }
      await client.query("UPDATE social_events SET status = 'processed' WHERE turn_id = $1", [turnId]);
      await outbox.enqueueMessages(client, turnId, payload.chatId ?? "0", result.messages);
      await applyDeltas(
        client,
        item.id,
        result.notes.map((text) => ({ type: "append_note" as const, text })),
      );
    } else {
      await applyDeltas(
        client,
        item.id,
        result.notes.map((text) => ({ type: "append_note" as const, text })),
      );
    }
  });

  logger.info("ход выполнен", {
    turnId: turnId,
    decision: result.decision,
    messages: result.messages.length,
    reason: result.reason,
  });
};

const handlers: Record<string, WorkHandler> = {
  consolidation: async (item) => {
    await withTransaction(pool, (client) =>
      applyDeltas(client, item.id, [
        { type: "append_note", text: `maintenance tick ${new Date().toISOString()}` },
      ]),
    );
  },
  direct: handleTurn,
  group: handleTurn,
};

const worker = new CognitiveWorker(repo, handlers, logger);
const scheduler = new BackgroundScheduler(
  { enqueue: (kind) => repo.enqueue(kind, {}).then(() => undefined), logger, budget: new BudgetRepo(pool) },
);

// Telegram-слой: polling, буферы, доставка
let poller: TelegramPoller | null = null;
let buffers: ChatBufferManager | null = null;
let delivery: DeliveryLoop | null = null;

if (config.telegram) {
  const telegramApi: TelegramApi = config.telegram.dryRun
    ? new DryRunTelegramApi(new URL("../../../.dry-run-updates.jsonl", import.meta.url).pathname.replace(/^\/([A-Z]:)/, "$1"), (line) =>
        console.log(JSON.stringify({ ts: new Date().toISOString(), level: "info", msg: line })),
      )
    : new HttpTelegramApi(config.telegram.botToken!, config.telegram.apiBase);

  buffers = new ChatBufferManager({ pool, logger });
  const recoveredBuffers = await buffers.recoverStalePending();
  if (recoveredBuffers.events > 0) {
    logger.info("recovery буферов: pending-события закрыты в turn", recoveredBuffers);
  }
  const recoveredSending = await outbox.recoverStaleSending();
  if (recoveredSending > 0) {
    logger.info("outbox recovery: sending → unknown", { count: recoveredSending });
  }

  delivery = new DeliveryLoop({ pool, repo: outbox, api: telegramApi, logger });
  poller = new TelegramPoller({
    pool,
    api: telegramApi,
    logger,
    onPendingEvent: (event) => buffers!.add(event),
  });
  worker.start();
  scheduler.start();
  delivery.start();
  await poller.start();
} else {
  logger.warn("Telegram не настроен (TELEGRAM_BOT_TOKEN или TELEGRAM_DRY_RUN=true) — polling и доставка выключены");
  worker.start();
  scheduler.start();
}

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
  if (poller) await poller.stop();
  if (buffers) await buffers.flushAll(); // незакрытые порции → turn
  scheduler.stop();
  await worker.stop();
  if (delivery) await delivery.stop();
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

logger.info("запуск завершён; polling, worker, планировщик и доставка работают");
