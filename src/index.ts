import { loadConfig } from "./runtime/config.js";
import { createLogger } from "./runtime/logger.js";
import { pingDatabase } from "./runtime/db-ping.js";

const config = loadConfig();
const logger = createLogger(config.logLevel);
const startedAt = Date.now();

logger.info("dio-agent запускается", {
  nodeEnv: config.nodeEnv,
  logLevel: config.logLevel,
});

const dbOk = await pingDatabase(config.databaseUrl, logger);
logger.info(dbOk ? "PostgreSQL доступен" : "PostgreSQL недоступен — работаю без БД (этап 0)");

const modules = ["telegram", "character", "memory", "background", "runtime"];
logger.info("модули объявлены (наполнение по этапам 1–5 чек-листа)", { modules });

const HEARTBEAT_MS = 15_000;
const heartbeat = setInterval(() => {
  logger.info("жив", { uptimeSec: Math.round((Date.now() - startedAt) / 1000) });
}, HEARTBEAT_MS);

function shutdown(signal: string): void {
  logger.info(`получен ${signal}, останавливаюсь`, {
    uptimeSec: Math.round((Date.now() - startedAt) / 1000),
  });
  clearInterval(heartbeat);
  // На этапе 1 здесь появится ограниченное ожидание текущих операций (§5.3).
  process.exit(0);
}

process.on("SIGINT", () => shutdown("SIGINT"));
process.on("SIGTERM", () => shutdown("SIGTERM"));

logger.info("запуск завершён; жду сообщений или SIGINT/SIGTERM");
