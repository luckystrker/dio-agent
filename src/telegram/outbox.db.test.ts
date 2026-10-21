// Интеграционный тест (живой PostgreSQL, база dio_test).
// §13.1: claimDue не отдаёт дважды (sending), порядок (chat_id, turn_id,
// position), not_before уважается; markSent/markRetryWait/markFailed/
// markUnknown + каскадная отмена зависимых (unknown → позиция+1..n
// cancelled); recoverStaleSending (§12.2: sending старого процесса →
// unknown, зависимые отменяются).
// PBT: enqueueMessages идемпотентен — UNIQUE(turn_id, position).

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import fc from "fast-check";
import type { Pool } from "pg";
import { OutboxRepo } from "./outbox.js";
import { withTransaction } from "../runtime/transactions.js";
import { createTestPool } from "../test-support/db.js";
import {
  cleanTelegramTables,
  createTurnRow,
  getOutboxByTurn,
  insertOutboxRow,
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

describe("claimDue (§13.1)", () => {
  it("не отдаёт дважды: захваченные строки в sending, повторный claim пуст", async () => {
    const turnId = await createTurnRow(pool, "500");
    await insertOutboxRow(pool, { turnId, chatId: "500", position: 0, text: "раз" });
    await insertOutboxRow(pool, { turnId, chatId: "500", position: 1, text: "два" });

    const first = await repo.claimDue(10);
    expect(first).toHaveLength(2);
    for (const row of first) {
      expect(row.status).toBe("sending");
      expect(row.attempts).toBe(1);
    }

    expect(await repo.claimDue(10)).toEqual([]); // ничего не отдаётся повторно

    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows.map((row) => row.status)).toEqual(["sending", "sending"]);
  });

  it("порядок (chat_id, turn_id, position) в возвращаемом массиве (регрессия находки Н1)", async () => {
    // Регрессия находки Н1: ORDER BY во внутреннем SELECT не переносится в
    // UPDATE ... RETURNING (физический порядок вставки) — поэтому claimDue
    // обязан пересортировать результат (outbox.ts:124-126, compareDeliveryOrder).
    const chat5Turn1 = await createTurnRow(pool, "5");
    const chat5Turn2 = await createTurnRow(pool, "5");
    const chat7Turn1 = await createTurnRow(pool, "7");
    // вставляем вперемешку — физический порядок не совпадает с сортировкой
    await insertOutboxRow(pool, { turnId: chat5Turn2, chatId: "5", position: 0, text: "x" });
    await insertOutboxRow(pool, { turnId: chat7Turn1, chatId: "7", position: 0, text: "x" });
    await insertOutboxRow(pool, { turnId: chat5Turn1, chatId: "5", position: 1, text: "x" });
    await insertOutboxRow(pool, { turnId: chat5Turn1, chatId: "5", position: 0, text: "x" });

    const claimed = await repo.claimDue(10);
    expect(claimed.map((row) => `${row.chatId}/${row.turnId}/${row.position}`)).toEqual([
      `5/${chat5Turn1}/0`,
      `5/${chat5Turn1}/1`,
      `5/${chat5Turn2}/0`,
      `7/${chat7Turn1}/0`,
    ]);
  });

  it("claimDue выдаёт правильный набор строк (порядок проверяется отдельным тестом выше)", async () => {
    const chat5Turn1 = await createTurnRow(pool, "5");
    const chat5Turn2 = await createTurnRow(pool, "5");
    const chat7Turn1 = await createTurnRow(pool, "7");
    await insertOutboxRow(pool, { turnId: chat5Turn2, chatId: "5", position: 0, text: "x" });
    await insertOutboxRow(pool, { turnId: chat7Turn1, chatId: "7", position: 0, text: "x" });
    await insertOutboxRow(pool, { turnId: chat5Turn1, chatId: "5", position: 1, text: "x" });
    await insertOutboxRow(pool, { turnId: chat5Turn1, chatId: "5", position: 0, text: "x" });

    const claimed = await repo.claimDue(10);
    const sortedKeys = claimed
      .map((row) => `${row.chatId}/${row.turnId}/${row.position}`)
      .sort();
    expect(sortedKeys).toEqual(
      [
        `5/${chat5Turn1}/0`,
        `5/${chat5Turn1}/1`,
        `5/${chat5Turn2}/0`,
        `7/${chat7Turn1}/0`,
      ].sort(),
    );
  });

  it("not_before уважается: будущие строки не выдаются, прошедшие — выдаются", async () => {
    const turnId = await createTurnRow(pool, "500");
    const futureId = await insertOutboxRow(pool, {
      turnId,
      chatId: "500",
      position: 0,
      text: "рано",
      notBeforeSql: "now() + interval '1 hour'",
    });
    const dueId = await insertOutboxRow(pool, { turnId, chatId: "500", position: 1, text: "пора" });

    const first = await repo.claimDue(10);
    expect(first.map((row) => row.id)).toEqual([dueId]);

    // будущее наступило (сдвигаем часы строки назад) — строка доступна
    await pool.query("UPDATE delivery_outbox SET not_before = now() - interval '1 second' WHERE id = $1", [futureId]);
    const second = await repo.claimDue(10);
    expect(second.map((row) => row.id)).toEqual([futureId]);
  });

  it("retry_wait с будущим not_before не выдаётся; с наступившим — выдаётся снова", async () => {
    const turnId = await createTurnRow(pool, "500");
    const waitId = await insertOutboxRow(pool, {
      turnId,
      chatId: "500",
      position: 0,
      text: "после 429",
      status: "retry_wait",
      notBeforeSql: "now() + interval '2 seconds'",
    });

    expect(await repo.claimDue(10)).toEqual([]);

    await pool.query("UPDATE delivery_outbox SET not_before = now() - interval '1 second' WHERE id = $1", [waitId]);
    const claimed = await repo.claimDue(10);
    expect(claimed.map((row) => row.id)).toEqual([waitId]);
    expect(claimed[0]!.status).toBe("sending");
    expect(claimed[0]!.attempts).toBe(1);
  });

  it("терминальные статусы (sent/unknown/cancelled/failed) не выдаются никогда", async () => {
    const turnId = await createTurnRow(pool, "500");
    for (const [position, status] of [
      [0, "sent"],
      [1, "unknown"],
      [2, "cancelled"],
      [3, "failed"],
    ] as const) {
      await insertOutboxRow(pool, { turnId, chatId: "500", position, text: "x", status });
    }
    expect(await repo.claimDue(10)).toEqual([]);
  });
});

describe("пометки исхода (§13.1)", () => {
  it("markSent: sent + telegram_message_id, last_error очищается (из статуса sending)", async () => {
    const turnId = await createTurnRow(pool, "500");
    const id = await insertOutboxRow(pool, { turnId, chatId: "500", position: 0, text: "x" });
    await repo.markRetryWait(id, 0, "временная"); // ошибка в прошлом, срок истёк — можно забрать
    const claimed = await repo.claimDue(1); // retry_wait→sending: markSent работает из sending
    expect(claimed).toHaveLength(1);
    await repo.markSent(id, "424242");

    const row = (await getOutboxByTurn(pool, turnId))[0]!;
    expect(row.status).toBe("sent");
    expect(row.telegram_message_id).toBe("424242");
    expect(row.last_error).toBeNull();
  });

  it("markSent не перезаписывает cancelled (условная запись, находка Н2)", async () => {
    const turnId = await createTurnRow(pool, "500");
    const id = await insertOutboxRow(pool, { turnId, chatId: "500", position: 0, text: "x" });
    await repo.claimDue(1);
    await repo.cancelTurnFrom(turnId, 0, "отменено тестом");
    await repo.markSent(id, "1");
    const row = (await getOutboxByTurn(pool, turnId))[0]!;
    expect(row.status).toBe("cancelled");
    expect(row.telegram_message_id).toBeNull();
  });

  it("markRetryWait: retry_wait + not_before в будущем на указанное время", async () => {
    const turnId = await createTurnRow(pool, "500");
    const id = await insertOutboxRow(pool, { turnId, chatId: "500", position: 0, text: "x" });

    await repo.markRetryWait(id, 3_000, "HTTP 429");

    const check = await pool.query<{ status: string; last_error: string; future: boolean }>(
      `SELECT status, last_error, not_before > now() + interval '2500 milliseconds' AS future
       FROM delivery_outbox WHERE id = $1`,
      [id],
    );
    expect(check.rows[0]).toMatchObject({ status: "retry_wait", last_error: "HTTP 429", future: true });
  });

  it("markFailed: failed + причина", async () => {
    const turnId = await createTurnRow(pool, "500");
    const id = await insertOutboxRow(pool, { turnId, chatId: "500", position: 0, text: "x" });

    await repo.markFailed(id, "HTTP 400");

    const row = (await getOutboxByTurn(pool, turnId))[0]!;
    expect(row.status).toBe("failed");
    expect(row.last_error).toBe("HTTP 400");
  });
});

describe("markUnknown: каскадная отмена зависимых (§13.2)", () => {
  it("unknown отменяет только последующие неотправленные позиции; sent/failed/unknown не трогает", async () => {
    const turnId = await createTurnRow(pool, "500");
    const pos0 = await insertOutboxRow(pool, { turnId, chatId: "500", position: 0, text: "ушло" });
    await pool.query("UPDATE delivery_outbox SET status = 'sent', telegram_message_id = 11 WHERE id = $1", [pos0]);
    const pos1 = await insertOutboxRow(pool, { turnId, chatId: "500", position: 1, text: "неопределённое" });
    await insertOutboxRow(pool, { turnId, chatId: "500", position: 2, text: "зависимое" });
    const pos3 = await insertOutboxRow(pool, { turnId, chatId: "500", position: 3, text: "уже failed" });
    await pool.query("UPDATE delivery_outbox SET status = 'failed' WHERE id = $1", [pos3]);

    const cancelled = await repo.markUnknown(pos1, "timeout после возможной отправки");
    expect(cancelled).toBe(1); // только позиция 2

    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows.map((row) => `${row.position}:${row.status}`)).toEqual([
      "0:sent",
      "1:unknown",
      "2:cancelled",
      "3:failed",
    ]);
    expect(rows[1]!.last_error).toContain("timeout");
    expect(rows[2]!.last_error).toBeTruthy(); // причина отмены зафиксирована
  });

  it("cancelTurnFrom отменяет с указанной позиции только выдающиеся статусы", async () => {
    const turnId = await createTurnRow(pool, "500");
    await insertOutboxRow(pool, { turnId, chatId: "500", position: 0, text: "ушло" });
    await insertOutboxRow(pool, { turnId, chatId: "500", position: 1, text: "pending" });
    const pos2 = await insertOutboxRow(pool, { turnId, chatId: "500", position: 2, text: "sending" });
    await pool.query("UPDATE delivery_outbox SET status = 'sending' WHERE id = $1", [pos2]);

    const cancelled = await repo.cancelTurnFrom(turnId, 1, "чат ушёл вперёд");
    expect(cancelled).toBe(2);

    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows.map((row) => row.status)).toEqual(["pending", "cancelled", "cancelled"]);
  });
});

describe("recoverStaleSending (§12.2: sending старого процесса)", () => {
  it("устаревший sending → unknown с отменой зависимых; свежий sending не трогается", async () => {
    const staleTurn = await createTurnRow(pool, "500");
    const staleId = await insertOutboxRow(pool, {
      turnId: staleTurn,
      chatId: "500",
      position: 0,
      text: "завис при отправке",
    });
    await insertOutboxRow(pool, { turnId: staleTurn, chatId: "500", position: 1, text: "зависимое" });
    // устаревает только захват (sending), зависимая позиция остаётся pending
    await pool.query(
      "UPDATE delivery_outbox SET status = 'sending', updated_at = now() - interval '2 minutes' WHERE id = $1",
      [staleId],
    );

    const freshTurn = await createTurnRow(pool, "501");
    const freshId = await insertOutboxRow(pool, {
      turnId: freshTurn,
      chatId: "501",
      position: 0,
      text: "отправляется прямо сейчас",
    });
    await pool.query("UPDATE delivery_outbox SET status = 'sending' WHERE id = $1", [freshId]);

    const recovered = await repo.recoverStaleSending(60_000);
    expect(recovered).toBe(1); // только устаревший

    const staleRows = await getOutboxByTurn(pool, staleTurn);
    expect(staleRows.map((row) => row.status)).toEqual(["unknown", "cancelled"]);
    expect(staleRows[0]!.last_error).toBeTruthy();

    const freshRow = (await getOutboxByTurn(pool, freshTurn))[0]!;
    expect(freshRow.status).toBe("sending"); // активная отправка не убита
  });

  it("без sending-строк recovery ничего не делает", async () => {
    const turnId = await createTurnRow(pool, "500");
    await insertOutboxRow(pool, { turnId, chatId: "500", position: 0, text: "x" });
    expect(await repo.recoverStaleSending(60_000)).toBe(0);
  });
});

describe("enqueueMessages: интервалы пузырей (§13.3)", () => {
  it("позиция p получает not_before ≈ created_at + p * 800 мс (дефолтный bubbleDelayMs)", async () => {
    const turnId = await createTurnRow(pool, "500");

    const inserted = await withTransaction(pool, (client) =>
      repo.enqueueMessages(client, turnId, "500", ["раз", "два", "три"]),
    );
    expect(inserted).toBe(3);

    const rows = await pool.query<{ position: number; delta_ms: number }>(
      `SELECT position, (EXTRACT(EPOCH FROM (not_before - created_at)) * 1000)::float8 AS delta_ms
       FROM delivery_outbox WHERE turn_id = $1 ORDER BY position`,
      [turnId],
    );
    expect(rows.rows.map((row) => row.position)).toEqual([0, 1, 2]);
    for (const row of rows.rows) {
      const expected = row.position * 800;
      // now() в одном INSERT — одна транзакционная метка: допуск только на
      // точность float
      expect(row.delta_ms).toBeGreaterThanOrEqual(expected - 5);
      expect(row.delta_ms).toBeLessThanOrEqual(expected + 5);
    }
    // позиция 0 доступна сразу, хвост — нет (интервалы реально отодвигают)
    const claimed = await repo.claimDue(10);
    expect(claimed.map((row) => row.position)).toEqual([0]);
  });
});

describe("PBT: enqueueMessages — идемпотентность применения (§12.1 п.5)", () => {
  it("любой список текстов: позиции 0..n-1 уникальны; повторный вызов не создаёт дублей", async () => {
    const textsArb = fc.array(fc.string({ minLength: 1, maxLength: 30 }), { maxLength: 6 });
    await fc.assert(
      fc.asyncProperty(textsArb, async (texts) => {
        const turnId = await createTurnRow(pool, "500");

        const inserted = await withTransaction(pool, (client) =>
          repo.enqueueMessages(client, turnId, "500", texts),
        );
        expect(inserted).toBe(texts.length);

        const rows = await getOutboxByTurn(pool, turnId);
        expect(rows.map((row) => row.position)).toEqual(texts.map((_, index) => index));
        expect(rows.map((row) => row.payload.text)).toEqual(texts);

        // повторное применение того же turn: конфликт (turn_id, position) молчит
        const again = await withTransaction(pool, (client) =>
          repo.enqueueMessages(client, turnId, "500", texts),
        );
        expect(again).toBe(0);
        expect(await getOutboxByTurn(pool, turnId)).toHaveLength(texts.length);
      }),
      { numRuns: 25 },
    );
  });
});
