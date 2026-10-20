import "dotenv/config";

export type LogLevel = "debug" | "info" | "warn" | "error";
export type NodeEnv = "development" | "production" | "test";

export interface ModelSettings {
  endpoint: string;
  apiKey: string;
  model: string;
  timeoutMs: number;
  maxRetries: number;
  maxOutputTokens: number;
}

export interface AppConfig {
  databaseUrl: string;
  logLevel: LogLevel;
  nodeEnv: NodeEnv;
  /** null — модель не настроена: ходы будут помечаться пропуском с причиной. */
  model: ModelSettings | null;
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

  const model = parseModelSettings(env);

  return {
    databaseUrl,
    logLevel: logLevelRaw as LogLevel,
    nodeEnv: nodeEnvRaw as NodeEnv,
    model,
  };
}

function parseModelSettings(env: NodeJS.ProcessEnv): ModelSettings | null {
  const endpoint = env["MODEL_ENDPOINT"]?.trim();
  const apiKey = env["MODEL_API_KEY"]?.trim();
  const model = env["MODEL_MODEL"]?.trim();
  const present = [endpoint, apiKey, model].filter((value) => value && value.length > 0);

  if (present.length === 0) return null;
  if (present.length < 3) {
    throw new ConfigError(
      `Модель настроена частично (${present.length} из 3): нужны MODEL_ENDPOINT, MODEL_API_KEY и MODEL_MODEL вместе — или уберите все три.`,
    );
  }

  const positiveInt = (raw: string | undefined, fallback: number): number => {
    const parsed = raw === undefined || raw.trim() === "" ? fallback : Number(raw);
    if (!Number.isInteger(parsed) || parsed <= 0) {
      throw new ConfigError(`значение "${raw}" должно быть целым положительным числом`);
    }
    return parsed;
  };

  return {
    endpoint: endpoint!,
    apiKey: apiKey!,
    model: model!,
    timeoutMs: positiveInt(env["MODEL_TIMEOUT_MS"], 60_000),
    maxRetries: positiveInt(env["MODEL_MAX_RETRIES"], 1),
    maxOutputTokens: positiveInt(env["MODEL_MAX_OUTPUT_TOKENS"], 1000),
  };
}
