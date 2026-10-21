// Интеграционный тест (живой PostgreSQL, база dio_test).
// §6.1 + инвариант 2: короткая транзакция ingest; UNIQUE(telegram_update_id)
// делает двойную пачку «уже сохранено», а не ошибкой; offset в той же
// транзакции, что событие; при ошибке вставки rollback откатывает и offset;
// GREATEST не даёт offset двигаться назад; порядок обработки — update_id.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import fc from "fast-check";
import type { Pool } from "pg";
import { ingestUpdate, readOffset } from "./ingest.js";
import { createTestPool } from "../test-support/db.js";
import { cleanTelegramTables as cleanAll } from "../test-support/telegram-db.js";

let pool: Pool;

beforeEach(async () => {
  pool = createTestPool();
  await cleanAll(pool);
});

afterEach(async () => {
  await pool.end();
});

function privateUpdate(updateId: number, text = "привет"): unknown {
  return {
    update_id: updateId,
    message: {
      message_id: updateId,
      from: { id: 42, is_bot: false },
      chat: { id: 500, type: "private" },
      date: 1_700_000_000,
      text,
    },
  };
}

function groupUpdate(updateId: number, kind: "chatter" | "reply"): unknown {
  return {
    update_id: updateId,
    message: {
      message_id: updateId,
      from: { id: 7, is_bot: false },
      chat: { id: -100_200, type: "supergroup" },
      date: 1_700_000_000,
      text: kind === "reply" ? "ответ тебе" : "просто шум",
      ...(kind === "reply" ? { reply_to_message: { message_id: 1 } } : {}),
    },
  };
}

async function eventsCount(): Promise<number> {
  const result = await pool.query<{ n: string }>("SELECT count(*)::text AS n FROM social_events");
  return Number(result.rows[0]!.n);
}

async function eventByUpdateId(updateId: number) {
  const result = await pool.query<{
    id: string;
    status: string;
    kind: string;
    chat_id: string;
    raw_update: unknown;
  }>(
    "SELECT id::text, status, kind, chat_id::text, raw_update FROM social_events WHERE telegram_update_id = $1",
    [updateId],
  );
  return result.rows[0];
}

describe("ingest: идемпотентность двойной пачки (§6.1 шаг 3, инвариант 2)", () => {
  it("вторая подача той же пачки вставляет 0 строк, конфликт не ошибка, offset не двигается", async () => {
    const batch = [privateUpdate(100), privateUpdate(101), privateUpdate(102)];

    for (const update of batch) {
      const result = await ingestUpdate(pool, update);
      expect(result.inserted).toBe(true);
      expect(result.eventId).not.toBeNull();
    }
    expect(await eventsCount()).toBe(3);
    expect(await readOffset(pool)).toBe(103);

    for (const update of batch) {
      const result = await ingestUpdate(pool, update);
      expect(result.inserted).toBe(false); // «уже сохранено», не ошибка
      expect(result.eventId).toBeNull();
      expect(result.status).toBe("pending");
    }
    expect(await eventsCount()).toBe(3); // ни одной новой строки
    expect(await readOffset(pool)).toBe(103); // offset стоит на месте
  });

  it("PBT: любой список updates с уникальными id — повторные подачи не создают дублей, offset = max+1", async () => {
    const idsArb = fc.uniqueArray(fc.integer({ min: 1, max: 5_000 }), { maxLength: 8 });
    await fc.assert(
      fc.asyncProperty(idsArb, async (ids) => {
        await cleanAll(pool);
        const sorted = [...ids].sort((a, b) => a - b);
        const updates = sorted.map((id) => privateUpdate(id, `текст ${id}`));

        for (const update of updates) {
          await ingestUpdate(pool, update);
        }
        // повторная подача той же пачки, затем в обратном порядке (телега может
        // прислать перемешанный остаток) — событий ровно столько же
        for (const update of updates) {
          const again = await ingestUpdate(pool, update);
          expect(again.inserted).toBe(false);
        }
        for (const update of [...updates].reverse()) {
          const again = await ingestUpdate(pool, update);
          expect(again.inserted).toBe(false);
        }

        const count = Number(
          (await pool.query<{ n: string }>("SELECT count(*)::text AS n FROM social_events")).rows[0]!.n,
        );
        expect(count).toBe(ids.length);
        const expectedOffset = sorted.length === 0 ? null : (sorted.at(-1) ?? 0) + 1;
        expect(await readOffset(pool)).toBe(expectedOffset);
      }),
      { numRuns: 20 },
    );
  });
});

describe("ingest: offset-атомарность (§6.1, инвариант 2)", () => {
  it("ошибка вставки события откатывает и offset: события нет, poll_state нет", async () => {
    // raw_update с BigInt ломает вставку (JSON.stringify падает внутри транзакции)
    const broken = {
      update_id: 10,
      message: {
        message_id: 10,
        from: { id: 1 },
        chat: { id: 9, type: "private" },
        text: "сломанное",
        junk: 5n,
      },
    };

    await expect(ingestUpdate(pool, broken)).rejects.toThrow();
    // rollback реально откатил: ни события, ни строки poll_state (offset не создан)
    expect(await eventByUpdateId(10)).toBeUndefined();
    expect(await readOffset(pool)).toBeNull();
  });

  it("сбой после успешной записи не двигает offset за упавшее событие", async () => {
    await ingestUpdate(pool, privateUpdate(300, "ок"));
    expect(await readOffset(pool)).toBe(301);

    const broken = {
      update_id: 305,
      message: {
        message_id: 305,
        from: { id: 1 },
        chat: { id: 9, type: "private" },
        text: "упадёт",
        junk: 7n,
      },
    };
    await expect(ingestUpdate(pool, broken)).rejects.toThrow();

    expect(await eventByUpdateId(305)).toBeUndefined(); // событие откатилось
    expect(await readOffset(pool)).toBe(301); // offset не продвинулся за 305
    expect(await eventsCount()).toBe(1);
  });

  it("GREATEST: поздний update не двигает offset назад при повторной подаче более раннего", async () => {
    await ingestUpdate(pool, privateUpdate(200));
    expect(await readOffset(pool)).toBe(201);

    // более ранний update (например, дубликат старой пачки) — событие новое, offset не назад
    const result = await ingestUpdate(pool, privateUpdate(150));
    expect(result.inserted).toBe(true);
    expect(await readOffset(pool)).toBe(201);
  });
});

describe("ingest: журнал и порядок (§6.1)", () => {
  it("неподдерживаемый callback_query фиксируется journal_only, offset не застревает", async () => {
    const result = await ingestUpdate(pool, { update_id: 77, callback_query: { id: "cb" } });
    expect(result.inserted).toBe(true);
    expect(result.status).toBe("journal_only");

    const row = await eventByUpdateId(77);
    expect(row?.status).toBe("journal_only");
    expect(row?.kind).toBe("other");
    expect(row?.chat_id).toBe("0"); // фиктивный чат из нормализатора
    expect(await readOffset(pool)).toBe(78);
  });

  it("статусы prefilter применяются на записи: группа-шум journal_only, reply pending, личка pending", async () => {
    await ingestUpdate(pool, groupUpdate(50, "chatter"));
    await ingestUpdate(pool, groupUpdate(51, "reply"));
    await ingestUpdate(pool, privateUpdate(52));

    expect((await eventByUpdateId(50))?.status).toBe("journal_only");
    expect((await eventByUpdateId(51))?.status).toBe("pending");
    expect((await eventByUpdateId(52))?.status).toBe("pending");
  });

  it("updates обрабатываются в порядке update_id: порядок id строк совпадает с порядком update_id", async () => {
    for (const id of [10, 11, 12, 13]) {
      await ingestUpdate(pool, privateUpdate(id, `сообщение ${id}`));
    }
    const result = await pool.query<{ telegram_update_id: string }>(
      "SELECT telegram_update_id::text FROM social_events ORDER BY id ASC",
    );
    // id строк отражает порядок обработки: если бы порядок нарушался,
    // последовательность update_id была бы не возрастающей
    expect(result.rows.map((row) => Number(row.telegram_update_id))).toEqual([10, 11, 12, 13]);
  });

  it("raw_update сохраняется полностью (журнал — источник истины, инвариант 3)", async () => {
    await ingestUpdate(pool, privateUpdate(90, "оригинал"));
    const row = await eventByUpdateId(90);
    expect(row?.raw_update).toMatchObject({
      update_id: 90,
      message: { text: "оригинал", chat: { id: 500 } },
    });
  });
});
