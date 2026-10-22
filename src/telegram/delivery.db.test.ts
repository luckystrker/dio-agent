// Интеграционный тест (живой PostgreSQL, база dio_test).
// §13.2/§13.3: DeliveryLoop с фейковым TelegramApi — успех → sent;
// сетевая ошибка/timeout → unknown + отмена зависимых (без автоповтора);
// 429 с retry_after → retry_wait на указанное время; постоянная 4xx →
// failed; устаревание (новые pending-события в чате после turn) →
// отмена оставшихся пузырей.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { DeliveryLoop } from "./delivery.js";
import { OutboxRepo } from "./outbox.js";
import { TelegramApiError, type TelegramApi, type SendMessageResult, type TelegramUpdate } from "./api.js";
import { withTransaction } from "../runtime/transactions.js";
import { createTestPool } from "../test-support/db.js";
import {
  cleanTelegramTables,
  createTurnRow,
  getOutboxByTurn,
  insertEvent,
  silentLogger,
} from "../test-support/telegram-db.js";

let pool: Pool;
let repo: OutboxRepo;

beforeEach(async () => {
  pool = createTestPool();
  repo = new OutboxRepo(pool);
  await cleanTelegramTables(pool);
});

afterEach(async () => {
  await pool.end();
});

type SendBehavior =
  | { kind: "ok" }
  | { kind: "net-error" }
  | { kind: "hang" }
  | { kind: "api-error"; status: number; retryAfterSec?: number };

class FakeApi implements TelegramApi {
  public readonly calls: Array<{ chatId: string; text: string }> = [];
  public readonly reactions: Array<{ chatId: string; messageId: string; emoji: string }> = [];
  private scriptIndex = 0;

  constructor(private readonly script: SendBehavior[] = []) {}

  async sendChatAction(): Promise<void> {
    // typing вне зоны этих тестов
  }

  async setMessageReaction(chatId: string, messageId: string, emoji: string): Promise<void> {
    this.reactions.push({ chatId, messageId, emoji });
  }

  async getUpdates(): Promise<TelegramUpdate[]> {
    return [];
  }

  async sendMessage(chatId: string, text: string, _signal: AbortSignal): Promise<SendMessageResult> {
    this.calls.push({ chatId, text });
    const behavior = this.script[this.scriptIndex++] ?? { kind: "ok" };
    if (behavior.kind === "net-error") throw new Error("ECONNRESET: соединение сброшено");
    if (behavior.kind === "hang") return new Promise<SendMessageResult>(() => undefined); // ответа не будет
    if (behavior.kind === "api-error") {
      throw new TelegramApiError(
        `sendMessage: HTTP ${behavior.status}`,
        behavior.status,
        behavior.retryAfterSec ?? null,
      );
    }
    return { messageId: String(424_200 + this.calls.length) };
  }
}

function makeLoop(api: FakeApi, options: { batchSize?: number; sendTimeoutMs?: number; maxAttempts?: number } = {}): DeliveryLoop {
  return new DeliveryLoop(
    { pool, repo, api, logger: silentLogger },
    { intervalMs: 600_000, batchSize: options.batchSize ?? 10, sendTimeoutMs: options.sendTimeoutMs ?? 5_000, maxAttempts: options.maxAttempts ?? 5 },
  );
}

/** Пузыри одного turn: все с not_before = now (delay 0). */
async function enqueueBubbles(turnId: string, chatId: string, texts: string[]): Promise<void> {
  const inserted = await withTransaction(pool, (client) =>
    repo.enqueueMessages(client, turnId, chatId, texts, 0),
  );
  expect(inserted).toBe(texts.length);
}

describe("DeliveryLoop: успех (§13.1)", () => {
  it("успешная отправка → sent с telegram_message_id, api вызван с текстом пузыря", async () => {
    const turnId = await createTurnRow(pool, "500");
    await enqueueBubbles(turnId, "500", ["первый пузырь", "второй пузырь"]);

    const api = new FakeApi();
    const loop = makeLoop(api, { batchSize: 2 });
    await loop.tick();

    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows.map((row) => row.status)).toEqual(["sent", "sent"]);
    expect(rows.map((row) => row.telegram_message_id)).toEqual(["424201", "424202"]);
    expect(api.calls).toEqual([
      { chatId: "500", text: "первый пузырь" },
      { chatId: "500", text: "второй пузырь" },
    ]);
  });
});

describe("DeliveryLoop: неопределённая отправка (§13.2)", () => {
  it("сетевая ошибка → unknown, зависимые отменены, автоповтора нет", async () => {
    const turnId = await createTurnRow(pool, "500");
    await enqueueBubbles(turnId, "500", ["неопределённое", "зависимое"]);

    const api = new FakeApi([{ kind: "net-error" }]);
    const loop = makeLoop(api, { batchSize: 1 }); // за такт — только первый пузырь
    await loop.tick();
    await loop.tick(); // второй такт: зависимый уже cancelled, не отправляется

    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows[0]!.status).toBe("unknown");
    expect(rows[0]!.last_error).toContain("ECONNRESET");
    expect(rows[1]!.status).toBe("cancelled");
    expect(api.calls).toHaveLength(1); // зависимый не отправлялся
  });

  it("timeout после возможной отправки → unknown, зависимые отменены", async () => {
    const turnId = await createTurnRow(pool, "500");
    await enqueueBubbles(turnId, "500", ["зависнет", "зависимое"]);

    const api = new FakeApi([{ kind: "hang" }]);
    const loop = makeLoop(api, { batchSize: 1, sendTimeoutMs: 150 });
    await loop.tick();
    await loop.tick();

    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows[0]!.status).toBe("unknown");
    expect(rows[0]!.last_error).toContain("таймаут");
    expect(rows[1]!.status).toBe("cancelled");
    expect(api.calls).toHaveLength(1);
  });

  it("unknown в захваченной пачке отменяет зависимые ДО их отправки (регрессия находки Н2)", async () => {
    // Регрессия находки Н2: delivery.ts:88-99 — перед отправкой каждого
    // пузыря из захваченной пачки статус перепроверяется (statusOf);
    // первый пузырь ушёл в unknown и отменил второй в БД — отменённый
    // не отправляется, markSent не перезаписывает cancelled.
    const turnId = await createTurnRow(pool, "500");
    await enqueueBubbles(turnId, "500", ["неопределённое", "зависимое"]);

    const api = new FakeApi([{ kind: "net-error" }, { kind: "ok" }]);
    const loop = makeLoop(api, { batchSize: 10 });
    await loop.tick();

    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows[0]!.status).toBe("unknown");
    expect(rows[1]!.status).toBe("cancelled");
    expect(api.calls).toHaveLength(1); // зависимый не дошёл до сети
  });
});

describe("DeliveryLoop: подтверждённые ошибки API (§13.2)", () => {
  it("429 с retry_after → retry_wait на указанное время; not_before уважается", async () => {
    const turnId = await createTurnRow(pool, "500");
    await enqueueBubbles(turnId, "500", ["после паузы"]);

    const api = new FakeApi([{ kind: "api-error", status: 429, retryAfterSec: 3 }]);
    const loop = makeLoop(api);
    await loop.tick();

    const row = (await getOutboxByTurn(pool, turnId))[0]!;
    expect(row.status).toBe("retry_wait");
    expect(row.attempts).toBe(1);
    const future = await pool.query<{ future: boolean }>(
      "SELECT not_before > now() + interval '2500 milliseconds' AS future FROM delivery_outbox WHERE turn_id = $1",
      [turnId],
    );
    expect(future.rows[0]!.future).toBe(true);

    // до наступления not_before строка не выдаётся
    expect(await repo.claimDue(10)).toEqual([]);
    // время пришло — снова в работе
    await pool.query(
      "UPDATE delivery_outbox SET not_before = now() - interval '1 second' WHERE turn_id = $1",
      [turnId],
    );
    const reclaimed = await repo.claimDue(10);
    expect(reclaimed).toHaveLength(1);
    expect(reclaimed[0]!.attempts).toBe(2);
  });

  it("постоянная 4xx → failed без повторов", async () => {
    const turnId = await createTurnRow(pool, "500");
    await enqueueBubbles(turnId, "500", ["плохой запрос"]);

    const api = new FakeApi([{ kind: "api-error", status: 400 }]);
    const loop = makeLoop(api);
    await loop.tick();
    await loop.tick(); // повторного захвата быть не должно

    const row = (await getOutboxByTurn(pool, turnId))[0]!;
    expect(row.status).toBe("failed");
    expect(row.last_error).toContain("HTTP 400");
    expect(api.calls).toHaveLength(1);
  });

  it("5xx → retry_wait (временная), при исчерпании попыток — failed", async () => {
    const exhaustedTurn = await createTurnRow(pool, "500");
    await enqueueBubbles(exhaustedTurn, "500", ["пять попыток уже было"]);
    // attempts=4: claim поднимет до 5, markTemporary увидит исчерпание
    await pool.query("UPDATE delivery_outbox SET attempts = 4 WHERE turn_id = $1", [exhaustedTurn]);

    const api = new FakeApi([{ kind: "api-error", status: 503 }]);
    const loop = makeLoop(api, { maxAttempts: 5 });
    await loop.tick();

    const row = (await getOutboxByTurn(pool, exhaustedTurn))[0]!;
    expect(row.status).toBe("failed");
    expect(row.last_error).toContain("лимит попыток");

    // а обычная 5xx с запасом попыток — retry_wait
    const retryTurn = await createTurnRow(pool, "501");
    await enqueueBubbles(retryTurn, "501", ["сервер лежит"]);
    const api2 = new FakeApi([{ kind: "api-error", status: 503 }]);
    const loop2 = makeLoop(api2, { maxAttempts: 5 });
    await loop2.tick();
    const row2 = (await getOutboxByTurn(pool, retryTurn))[0]!;
    expect(row2.status).toBe("retry_wait");
  });
});

describe("DeliveryLoop: устаревание (§13.3)", () => {
  it("новые pending-события в чате после turn → оставшиеся пузыри отменяются, отправки нет", async () => {
    const turnId = await createTurnRow(pool, "500");
    await enqueueBubbles(turnId, "500", ["уже неактуально", "тоже отменяется"]);
    // пользователь написал снова, событие ещё не закрыто в turn
    await insertEvent(pool, {
      telegramUpdateId: 900,
      chatId: "500",
      chatType: "private",
      text: "новое сообщение",
      status: "pending",
    });

    const api = new FakeApi();
    const loop = makeLoop(api, { batchSize: 2 });
    await loop.tick();

    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows.map((row) => row.status)).toEqual(["cancelled", "cancelled"]);
    expect(api.calls).toHaveLength(0); // ни один пузырь не отправлен
  });

  it("отправленный ранее пузырь не отзывается; устаревание отменяет только хвост", async () => {
    const turnId = await createTurnRow(pool, "500");
    await enqueueBubbles(turnId, "500", ["успело уйти", "не успел"]);
    await pool.query(
      "UPDATE delivery_outbox SET status = 'sent', telegram_message_id = 7 WHERE turn_id = $1 AND position = 0",
      [turnId],
    );
    await insertEvent(pool, {
      telegramUpdateId: 901,
      chatId: "500",
      chatType: "private",
      text: "чат ушёл вперёд",
      status: "pending",
    });

    const api = new FakeApi();
    const loop = makeLoop(api, { batchSize: 2 });
    await loop.tick();

    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows[0]!.status).toBe("sent"); // отправленное не переписывается
    expect(rows[1]!.status).toBe("cancelled");
    expect(api.calls).toHaveLength(0);
  });

  it("события другого чата и закрытые в turn события не вызывают отмену", async () => {
    const turnId = await createTurnRow(pool, "500");
    await enqueueBubbles(turnId, "500", ["актуальный ответ"]);
    await insertEvent(pool, {
      telegramUpdateId: 902,
      chatId: "777",
      chatType: "private",
      text: "в другом чате",
      status: "pending",
    });
    await insertEvent(pool, {
      telegramUpdateId: 903,
      chatId: "500",
      chatType: "private",
      text: "уже в другом turn",
      status: "buffered",
    });

    const api = new FakeApi();
    const loop = makeLoop(api);
    await loop.tick();

    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows[0]!.status).toBe("sent");
    expect(api.calls).toEqual([{ chatId: "500", text: "актуальный ответ" }]);
  });
});
