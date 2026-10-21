import { describe, expect, it } from "vitest";
import { ConfigError, loadConfig } from "./config.js";

const VALID_ENV = { DATABASE_URL: "postgres://user:pass@localhost:5432/dio" };

describe("loadConfig", () => {
  it("принимает минимальный env и ставит дефолты", () => {
    const config = loadConfig({ ...VALID_ENV });
    expect(config.databaseUrl).toBe(VALID_ENV["DATABASE_URL"]);
    expect(config.logLevel).toBe("info");
    expect(config.nodeEnv).toBe("development");
  });

  it("читает LOG_LEVEL и NODE_ENV из env", () => {
    const config = loadConfig({
      ...VALID_ENV,
      LOG_LEVEL: "debug",
      NODE_ENV: "production",
    });
    expect(config.logLevel).toBe("debug");
    expect(config.nodeEnv).toBe("production");
  });

  it("без DATABASE_URL бросает ConfigError с подсказкой про .env.example", () => {
    expect(() => loadConfig({})).toThrow(ConfigError);
    expect(() => loadConfig({})).toThrow(/\.env\.example/);
  });

  it("пустой DATABASE_URL (одни пробелы) тоже отклоняется", () => {
    expect(() => loadConfig({ DATABASE_URL: "   " })).toThrow(ConfigError);
  });

  it("неизвестный LOG_LEVEL отклоняется с перечислением допустимых", () => {
    expect(() => loadConfig({ ...VALID_ENV, LOG_LEVEL: "loud" })).toThrow(
      /debug, info, warn, error/,
    );
  });

  it("неизвестный NODE_ENV отклоняется", () => {
    expect(() => loadConfig({ ...VALID_ENV, NODE_ENV: "staging" })).toThrow(
      ConfigError,
    );
  });

  it("без MODEL_* модель не настроена (null), без ошибок", () => {
    expect(loadConfig({ ...VALID_ENV }).model).toBeNull();
  });

  it("полный набор MODEL_* парсится с дефолтами", () => {
    const model = loadConfig({
      ...VALID_ENV,
      MODEL_ENDPOINT: "https://api.example/v1",
      MODEL_API_KEY: "sk",
      MODEL_MODEL: "m-1",
    }).model;
    expect(model).toEqual({
      endpoint: "https://api.example/v1",
      apiKey: "sk",
      model: "m-1",
      timeoutMs: 60_000,
      maxRetries: 1,
      maxOutputTokens: 1000,
      dailyCallsLimit: 100,
    });
  });

  it("частичный набор MODEL_* отклоняется с перечислением трёх переменных", () => {
    expect(() =>
      loadConfig({ ...VALID_ENV, MODEL_ENDPOINT: "https://api.example/v1" }),
    ).toThrow(/MODEL_ENDPOINT, MODEL_API_KEY и MODEL_MODEL/);
  });

  it("нецелые числовые настройки модели отклоняются", () => {
    expect(() =>
      loadConfig({
        ...VALID_ENV,
        MODEL_ENDPOINT: "https://api.example/v1",
        MODEL_API_KEY: "sk",
        MODEL_MODEL: "m",
        MODEL_TIMEOUT_MS: "soon",
      }),
    ).toThrow(ConfigError);
  });
});
