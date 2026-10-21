import { loadConfig } from "./runtime/config.js";
import { createLogger } from "./runtime/logger.js";
import { createDatabasePool } from "./runtime/db.js";
import { acquireSingleton, SingletonTakenError, type Singleton } from "./runtime/singleton.js";
import { WorkQueueRepo } from "./runtime/work-queue.js";
import { CognitiveWorker, type WorkHandler } from "./runtime/worker.js";
import { BackgroundScheduler } from "./runtime/scheduler.js";
import { BudgetRepo } from "./runtime/budgets.js";
import { withTransaction } from "./runtime/transactions.js";
import { applyDeltas } from "./runtime/deltas.js";
import { OpenAICompatModelAdapter, type CharacterModel } from "./character/model-adapter.js";
import { makeTurnHandler } from "./character/turn-handler.js";
import { MemoryRepo } from "./memory/memory-repo.js";
import { consolidateEpisodes } from "./memory/consolidation.js";
import { IntentionRepo } from "./background/intentions.js";
import { makeIntentionHandler } from "./background/intention-handler.js";
import { InitiativeRepo } from "./character/initiative.js";
import { PersonaRepo } from "./character/persona.js";
import { MoodRepo } from "./character/mood.js";
import { RelationshipRepo } from "./character/relationships.js";
import { DryRunTelegramApi, HttpTelegramApi, type TelegramApi } from "./telegram/api.js";
import { fileURLToPath } from "node:url";
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

const memory = new MemoryRepo(pool);
const personaRepo = new PersonaRepo(pool);
const persona = await personaRepo.active();
const moodRepo = new MoodRepo(pool);
const relationshipRepo = new RelationshipRepo(pool);
const modelBudget = new BudgetRepo(pool);
const intentionsRepo = new IntentionRepo(pool);
const initiativeRepo = new InitiativeRepo(pool);
const handleTurn = makeTurnHandler({
  pool,
  model,
  outbox,
  memory,
  intentions: intentionsRepo,
  persona,
  mood: moodRepo,
  relationships: relationshipRepo,
  workQueue: repo,
  modelBudget: config.model ? modelBudget : null,
  modelDailyCallsLimit: config.model?.dailyCallsLimit ?? 100,
  logger,
});

const handlers: Record<string, WorkHandler> = {
  consolidation: async (item) => {
    // §10.2: episode summaries закрытых эпизодов + служебная заметка о такте
    const result = await consolidateEpisodes(pool);
    await withTransaction(pool, (client) =>
      applyDeltas(client, item.id, [
        {
          type: "append_note",
          text: `consolidation tick ${new Date().toISOString()}: эпизодов ${result.chats}, событий ${result.events}`,
        },
      ]),
    );
    logger.info("консолидация эпизодов", result);
  },
  direct: handleTurn,
  group: handleTurn,
  intention: makeIntentionHandler({
    pool,
    model,
    memory,
    persona,
    outbox,
    intentions: intentionsRepo,
    initiative: initiativeRepo,
    workQueue: repo,
    modelBudget: config.model ? modelBudget : null,
    modelDailyCallsLimit: config.model?.dailyCallsLimit ?? 100,
    logger,
  }),
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
    ? new DryRunTelegramApi(fileURLToPath(new URL("../.dry-run-updates.jsonl", import.meta.url)), (line) =>
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

// §14.2: планировщик намерений — проверка due-работ каждые 30 секунд,
// сама проверка LLM не вызывает
const INTENTION_TICK_MS = 30_000;
const intentionTicker = setInterval(() => {
  void intentionsRepo
    .claimDue()
    .then((count) => {
      if (count > 0) logger.info("намерения переданы в работу", { count });
    })
    .catch((error: unknown) => {
      logger.error("тикер намерений: ошибка", {
        error: error instanceof Error ? error.message : String(error),
      });
    });
}, INTENTION_TICK_MS);

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
  clearInterval(intentionTicker);
  // 1.11: каждый шаг в своей защите; exit-код выставляется всегда
  try {
    try {
      if (poller) await poller.stop();
    } catch (error) {
      logger.error("shutdown: poller", { error: String(error) });
    }
    try {
      if (buffers) await buffers.flushAll();
    } catch (error) {
      logger.error("shutdown: buffers", { error: String(error) });
    }
    scheduler.stop();
    try {
      await worker.stop();
    } catch (error) {
      logger.error("shutdown: worker", { error: String(error) });
    }
    try {
      if (delivery) await delivery.stop();
    } catch (error) {
      logger.error("shutdown: delivery", { error: String(error) });
    }
    try {
      await singleton.release();
    } catch (error) {
      logger.error("shutdown: singleton", { error: String(error) });
    }
  } finally {
    await pool.end().catch(() => undefined);
    process.exit(exitCode);
  }
}

singleton.onLost((reason) => void shutdown(`потеряно singleton-соединение: ${reason}`, 4));
process.on("SIGINT", () => void shutdown("SIGINT"));
process.on("SIGTERM", () => void shutdown("SIGTERM"));

logger.info("запуск завершён; polling, worker, планировщик и доставка работают");
