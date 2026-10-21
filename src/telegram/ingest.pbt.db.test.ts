// PBT идемпотентности ingest (этап 6.3, §6.1, инвариант из §21.1 «сбой после
// сохранения update — дедупликация не создаёт второе событие»): любая
// сгенерированная пачка update'ов при повторном (и переставленном) прогоне
// не создаёт новых событий, не двигает offset назад и не меняет статусы.

import { beforeEach, afterEach, describe, expect, it } from "vitest";
import fc from "fast-check";
import type { Pool } from "pg";
import { ingestUpdate, readOffset } from "./ingest.js";
import { createTestPool } from "../test-support/db.js";
import { cleanTelegramTables } from "../test-support/telegram-db.js";

let pool: Pool;

beforeEach(async () => {
  pool = createTestPool();
  await cleanTelegramTables(pool);
});

afterEach(async () => {
  await pool.end();
});

const textArb = fc.string({ minLength: 1, maxLength: 40 }).filter((s) => s.trim().length > 0);
const chatIdArb = fc.integer({ min: 100_000, max: 999_999 });

interface ScriptedUpdate {
  updateId: number;
  chatId: number;
  text: string;
  raw: unknown;
}

function toRaw(update: ScriptedUpdate): unknown {
  return {
    update_id: update.updateId,
    message: {
      message_id: update.updateId,
      from: { id: 4242, is_bot: false },
      chat: { id: update.chatId, type: "private" },
      date: Math.floor(Date.now() / 1000),
      text: update.text,
    },
  };
}

const batchArb = fc
  .uniqueArray(fc.integer({ min: 1, max: 10_000 }), { minLength: 1, maxLength: 12 })
  .chain((updateIds) =>
    fc.record({
      chatId: chatIdArb,
      texts: fc.array(textArb, { minLength: updateIds.length, maxLength: updateIds.length }),
    }).map(({ chatId, texts }) => {
      const base = 100_000; // update_id из генератора — уникальны в рамках прогона
      const updates: ScriptedUpdate[] = updateIds.map((updateId, index) => ({
        updateId: base + updateId,
        chatId,
        text: texts[index]!,
        raw: undefined,
      }));
      updates.forEach((update) => (update.raw = toRaw(update)));
      return updates;
    }),
  );

describe("PBT: идемпотентность ingest (живая БД)", () => {
  it("повторный прогон той же пачки: событий столько же, offset не ниже, статусы не меняются", async () => {
    await fc.assert(
      fc.asyncProperty(batchArb, async (batch) => {
        await cleanTelegramTables(pool);
        for (const update of batch) await ingestUpdate(pool, update.raw);

        const eventsBefore = await snapshotEvents();
        const offsetBefore = await readOffset(pool);
        expect(eventsBefore.length).toBe(batch.length);

        // сбой после сохранения: та же пачка приходит второй раз
        for (const update of batch) await ingestUpdate(pool, update.raw);

        const eventsAfter = await snapshotEvents();
        const offsetAfter = await readOffset(pool);
        expect(eventsAfter).toEqual(eventsBefore);
        expect(offsetAfter).toBe(offsetBefore);
      }),
      { numRuns: 12 },
    );
  });

  it("повтор в обратном порядке: итог тот же, offset = max(update_id)+1", async () => {
    await fc.assert(
      fc.asyncProperty(batchArb, async (batch) => {
        await cleanTelegramTables(pool);
        for (const update of batch) await ingestUpdate(pool, update.raw);
        for (const update of [...batch].reverse()) await ingestUpdate(pool, update.raw);

        const events = await snapshotEvents();
        expect(events.length).toBe(batch.length);
        const offset = await readOffset(pool);
        expect(offset).toBe(Math.max(...batch.map((update) => update.updateId)) + 1);
      }),
      { numRuns: 12 },
    );
  });

  async function snapshotEvents(): Promise<Array<{ update_id: number; status: string }>> {
    const result = await pool.query<{ update_id: number; status: string }>(
      "SELECT telegram_update_id, status FROM social_events ORDER BY telegram_update_id",
    );
    return result.rows.map((row) => ({ update_id: Number(row.update_id), status: row.status }));
  }
});
