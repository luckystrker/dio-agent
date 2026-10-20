import "dotenv/config";

export type LogLevel = "debug" | "info" | "warn" | "error";
export type NodeEnv = "development" | "production" | "test";

export interface AppConfig {
  databaseUrl: string;
  logLevel: LogLevel;
  nodeEnv: NodeEnv;
}

export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}

const LOG_LEVELS: readonly LogLevel[] = ["debug", "info", "warn", "error"];
const NODE_ENVS: readonly NodeEnv[] = ["development", "production", "test"];

export function loadConfig(env: NodeJS.ProcessEnv = process.env): AppConfig {
  const databaseUrl = env["DATABASE_URL"]?.trim();
  if (!databaseUrl) {
    throw new ConfigError(
      "DATABASE_URL не задан. Скопируй .env.example в .env и заполни (PostgreSQL поднимается через docker compose).",
    );
  }

  const logLevelRaw = env["LOG_LEVEL"]?.trim() ?? "info";
  if (!LOG_LEVELS.includes(logLevelRaw as LogLevel)) {
    throw new ConfigError(
      `LOG_LEVEL="${logLevelRaw}" не поддерживается. Ожидаю один из: ${LOG_LEVELS.join(", ")}.`,
    );
  }

  const nodeEnvRaw = env["NODE_ENV"]?.trim() ?? "development";
  if (!NODE_ENVS.includes(nodeEnvRaw as NodeEnv)) {
    throw new ConfigError(
      `NODE_ENV="${nodeEnvRaw}" не поддерживается. Ожидаю один из: ${NODE_ENVS.join(", ")}.`,
    );
  }

  return {
    databaseUrl,
    logLevel: logLevelRaw as LogLevel,
    nodeEnv: nodeEnvRaw as NodeEnv,
  };
}
