// Интеграционный тест (живой PostgreSQL, база dio_test).
// §6.1 на уровне poller с фейковым TelegramApi: пачка обрабатывается в
// порядке update_id; при ошибке БД посреди пачки последующие updates этой
// пачки не обрабатываются, offset не двигается, после устранения сбоя
// poller продолжает с сохранённого offset; двойная подача пачки не создаёт
// дублей и не дёргает буферы повторно (инварианты 1 и 2).

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { TelegramPoller, type PendingEvent } from "./polling.js";
import { readOffset } from "./ingest.js";
import type { TelegramApi, TelegramUpdate } from "./api.js";
import { createTestPool, waitFor } from "../test-support/db.js";
import { cleanTelegramTables, silentLogger } from "../test-support/telegram-db.js";

let pool: Pool;

beforeEach(async () => {
  pool = createTestPool();
  await cleanTelegramTables(pool);
});

afterEach(async () => {
  await pool.end();
});

/** Фейковый API: держит фиксированный набор updates, фильтрует по offset
 *  как настоящий Telegram; в режиме duplicate выдаёт пачку повторно. */
class FakeApi implements TelegramApi {
  public readonly offsetCalls: number[] = [];
  private pollCount = 0;

  async sendChatAction(): Promise<void> {
    // typing вне зоны polling-тестов
  }

  async setMessageReaction(): Promise<void> {
    // реакции вне зоны polling-тестов
  }

  constructor(
    private readonly updates: TelegramUpdate[],
    private readonly options: { duplicate?: boolean } = {},
  ) {}

  async getUpdates(offset: number, _timeoutSec: number, _signal: AbortSignal): Promise<TelegramUpdate[]> {
    this.offsetCalls.push(offset);
    this.pollCount++;
    if (this.options.duplicate && this.pollCount <= 2) {
      return [...this.updates].sort((a, b) => a.update_id - b.update_id);
    }
    const due = this.updates
      .filter((update) => update.update_id >= offset)
      .sort((a, b) => a.update_id - b.update_id);
    if (due.length === 0) {
      // имитация long poll: не крутиться вхолостую на реальных таймерах
      await new Promise((resolve) => setTimeout(resolve, 5));
    }
    return due;
  }

  async sendMessage(): Promise<never> {
    throw new Error("polling-тест не использует sendMessage");
  }
}

function privateUpdate(updateId: number, text: string): TelegramUpdate {
  return {
    update_id: updateId,
    message: {
      message_id: updateId,
      from: { id: 42, is_bot: false },
      chat: { id: 500, type: "private" },
      date: 1_700_000_000,
      text,
    },
  } as TelegramUpdate;
}

function makePoller(api: TelegramApi, onPendingEvent: (event: PendingEvent) => void): TelegramPoller {
  return new TelegramPoller({ pool, api, logger: silentLogger, onPendingEvent }, { errorBackoffMs: 25 });
}

async function eventsCount(): Promise<number> {
  const result = await pool.query<{ n: string }>("SELECT count(*)::text AS n FROM social_events");
  return Number(result.rows[0]!.n);
}

describe("TelegramPoller: порядок и offset (§6.1)", () => {
  it("пачка обрабатывается в порядке update_id; события и колбэки буферов в том же порядке", async () => {
    const updates = [
      privateUpdate(30, "третье по id, но второе в массиве"),
      privateUpdate(20, "первое"),
      privateUpdate(40, "последнее"),
      privateUpdate(10, "нулевое"),
    ];
    const callbacks: string[] = [];
    const poller = makePoller(new FakeApi(updates), (event) => callbacks.push(event.text ?? ""));
    await poller.start();
    try {
      await waitFor(async () => (await eventsCount()) === 4, { label: "4 события" });
    } finally {
      await poller.stop();
    }

    // порядок строк журнала = порядок обработки = порядок update_id
    const rows = await pool.query<{ telegram_update_id: string }>(
      "SELECT telegram_update_id::text FROM social_events ORDER BY id ASC",
    );
    expect(rows.rows.map((row) => Number(row.telegram_update_id))).toEqual([10, 20, 30, 40]);
    // буферы разбужены в том же порядке (инвариант 1: без ожидания чего-либо)
    expect(callbacks).toEqual(["нулевое", "первое", "третье по id, но второе в массиве", "последнее"]);
    expect(await readOffset(pool)).toBe(41);
  });

  it("стартовый offset читается из БД, а не с нуля (§6.1 шаг 5 → следующий запуск)", async () => {
    await pool.query("INSERT INTO poll_state (key, value) VALUES ('telegram_offset', 777)");
    const api = new FakeApi([privateUpdate(800, "после рестарта")]);
    const poller = makePoller(api, () => undefined);
    await poller.start();
    try {
      await waitFor(async () => (await eventsCount()) === 1, { label: "событие после рестарта" });
    } finally {
      await poller.stop();
    }
    expect(api.offsetCalls[0]).toBe(777);
    expect(await readOffset(pool)).toBe(801);
  });
});

describe("TelegramPoller: сбой посреди пачки (§6.1)", () => {
  const TRIGGER_FN = "boom_ingest_test";
  const TRIGGER_SQL = `
    CREATE OR REPLACE FUNCTION ${TRIGGER_FN}() RETURNS trigger AS $fn$
    BEGIN
      IF NEW.text = 'boom' THEN RAISE EXCEPTION 'ингест: симулированный сбой БД'; END IF;
      RETURN NEW;
    END $fn$ LANGUAGE plpgsql`;
  const CREATE_TRIGGER = `CREATE TRIGGER trg_boom BEFORE INSERT ON social_events FOR EACH ROW EXECUTE FUNCTION ${TRIGGER_FN}()`;

  it("ошибка записи события: хвост пачки не обрабатывается, offset стоит, после устранения сбоя — продолжение", async () => {
    await pool.query(TRIGGER_SQL);
    await pool.query(CREATE_TRIGGER);
    try {
      const updates = [
        privateUpdate(10, "до сбоя"),
        privateUpdate(11, "boom"),
        privateUpdate(12, "после сбоя"),
      ];
      const callbacks: string[] = [];
      const poller = makePoller(new FakeApi(updates), (event) => callbacks.push(event.text ?? ""));
      await poller.start();
      try {
        await waitFor(async () => (await eventsCount()) === 1, { label: "первое событие сохранено" });

        // хвост пачки остановлен: 12 не вставлен, пока 11 падает; offset = 11
        expect(await eventsCount()).toBe(1);
        expect(await readOffset(pool)).toBe(11);

        // сбой устранён (например, транзиентная ошибка БД прошла)
        await pool.query("DROP TRIGGER trg_boom ON social_events");
        await waitFor(async () => (await eventsCount()) === 3, { label: "пачка доехала после retry" });
      } finally {
        await poller.stop();
      }

      expect(await readOffset(pool)).toBe(13);
      expect(callbacks).toEqual(["до сбоя", "boom", "после сбоя"]);
      const rows = await pool.query<{ telegram_update_id: string }>(
        "SELECT telegram_update_id::text FROM social_events ORDER BY id ASC",
      );
      expect(rows.rows.map((row) => Number(row.telegram_update_id))).toEqual([10, 11, 12]);
    } finally {
      await pool
        .query("DROP TRIGGER IF EXISTS trg_boom ON social_events")
        .catch(() => undefined);
      await pool
        .query(`DROP FUNCTION IF EXISTS ${TRIGGER_FN}()`)
        .catch(() => undefined);
    }
  });
});

describe("TelegramPoller: двойная пачка от API (§6.1 шаг 3)", () => {
  it("повторная выдача той же пачки не создаёт событий и не дёргает буферы второй раз", async () => {
    const updates = [privateUpdate(50, "раз"), privateUpdate(51, "два")];
    const callbacks: string[] = [];
    const api = new FakeApi(updates, { duplicate: true });
    const poller = makePoller(api, (event) => callbacks.push(event.text ?? ""));
    await poller.start();
    try {
      await waitFor(async () => api.offsetCalls.length >= 2, { label: "вторая выдача пачки" });
      await waitFor(async () => (await eventsCount()) === 2, { label: "события" });
    } finally {
      await poller.stop();
    }

    expect(await eventsCount()).toBe(2);
    expect(callbacks).toEqual(["раз", "два"]); // каждый update разбудил буфер ровно один раз
    expect(await readOffset(pool)).toBe(52);
  });
});
