// PBT закрытия буферов (этап 6.3, §6.2): любая сгенерированная
// последовательность входящих событий по двум чатам (без пауз между
// сообщениями, как при «потоке» подряд) разбивается на порции так, что:
// каждое событие попадает ровно в один turn (не потеряно, не задублировано),
// порядок событий чата сохраняется, нетраншейная порция обязана была
// закрыться по границе — ровно 20 событий или накопленные 8000 символов
// (хвостовой batch закрывается таймером idle/max и может быть любым).

import { beforeEach, afterEach, describe, expect, it } from "vitest";
import fc from "fast-check";
import type { Pool } from "pg";
import { MAX_BATCH_TEXT_CHARS, MAX_EVENTS_PER_BATCH } from "./buffers.js";
import { ChatBufferManager } from "./buffers.js";
import { createTestPool } from "../test-support/db.js";
import { cleanTelegramTables, insertEvent, waitForDb, silentLogger } from "../test-support/telegram-db.js";

let pool: Pool;

beforeEach(async () => {
  pool = createTestPool();
  await cleanTelegramTables(pool);
});

afterEach(async () => {
  await pool.end();
});

const CHAT_IDS = ["700001", "700002"];

interface ScriptedEvent {
  chatIndex: 0 | 1;
  text: string;
  eventId: number;
}

const eventListArb = fc
  .tuple(
    fc.array(fc.integer({ min: 1, max: 3000 }), { minLength: 2, maxLength: 45 }),
    fc.array(fc.integer({ min: 1, max: 3000 }), { minLength: 2, maxLength: 45 }),
  )
  .map(([lengthsA, lengthsB]) => {
    // чередуем события двух чатов, длины текста из генератора
    const script: ScriptedEvent[] = [];
    const queues = [lengthsA.slice(), lengthsB.slice()];
    const eventIdBase = 500_000;
    let eventId = 0;
    let chatIndex: 0 | 1 = 0;
    while (queues[0]!.length + queues[1]!.length > 0) {
      if (queues[chatIndex]!.length === 0) chatIndex = chatIndex === 0 ? 1 : 0;
      const length = queues[chatIndex]!.shift()!;
      script.push({ chatIndex, text: "а".repeat(length), eventId: eventIdBase + eventId++ });
      const other = chatIndex === 0 ? 1 : 0;
      if (queues[other]!.length > 0) chatIndex = other as 0 | 1; // чередуем, пока есть события
    }
    return script;
  });

describe("PBT: разбиение потока сообщений на порции (живая БД)", () => {
  it("каждое событие ровно в одном turn, порядок сохранён, нетраншейные порции закрыты по границе 20/8000", async () => {
    await fc.assert(
      fc.asyncProperty(eventListArb, async (script) => {
        await cleanTelegramTables(pool);
        const buffers = new ChatBufferManager({ pool, logger: silentLogger });

        for (const event of script) {
          const eventId = await insertEvent(pool, {
            telegramUpdateId: event.eventId,
            chatId: CHAT_IDS[event.chatIndex]!,
            chatType: "private",
            kind: "message",
            text: event.text,
          });
          buffers.add({
            eventId,
            chatId: CHAT_IDS[event.chatIndex]!,
            chatType: "private",
            kind: "message",
            text: event.text,
            occurredAt: new Date(),
          });
        }

        // все события в итоге привязаны к turn'ам (idle-окно ≤ 1.5 с, max ≤ 3 с)
        await waitForDb(
          async () => {
            const pending = await pool.query("SELECT id FROM social_events WHERE turn_id IS NULL");
            return pending.rows.length === 0;
          },
          "все события привязаны к turn",
          8_000,
        );

        for (const [chatIndex, chatId] of CHAT_IDS.entries()) {
          const rows = await pool.query<{ id: string; turn_id: string; text: string | null }>(
            `SELECT e.id::text, e.turn_id::text, e.text FROM social_events e
             WHERE e.chat_id = $1 ORDER BY e.id`,
            [chatId],
          );
          const chatEvents = script.filter((event) => event.chatIndex === chatIndex);
          expect(rows.rows.map((row) => row.text ?? "")).toEqual(chatEvents.map((event) => event.text));

          // порция = группа подряд идущих событий с одним turn_id
          const batches: Array<{ turnId: string; events: typeof rows.rows }> = [];
          for (const row of rows.rows) {
            const last = batches.at(-1);
            if (last && last.turnId === row.turn_id) last.events.push(row);
            else batches.push({ turnId: row.turn_id, events: [row] });
          }
          // один turn не встречается двумя непрерывными группами
          const turnIds = batches.map((batch) => batch.turnId);
          expect(new Set(turnIds).size).toBe(turnIds.length);

          batches.forEach((batch, index) => {
            const chars = batch.events.reduce((sum, event) => sum + (event.text?.length ?? 0), 0);
            expect(batch.events.length).toBeLessThanOrEqual(MAX_EVENTS_PER_BATCH);
            const isTail = index === batches.length - 1;
            if (!isTail) {
              // порция закрыта триггером: ровно 20 событий ИЛИ накоплено ≥8000 символов
              const byCount = batch.events.length === MAX_EVENTS_PER_BATCH;
              const byChars = chars >= MAX_BATCH_TEXT_CHARS;
              expect(byCount || byChars, `нетраншейная порция закрыта без границы: ${batch.events.length} событий, ${chars} символов`).toBe(true);
            }
          });
        }
      }),
      { numRuns: 10 },
    );
  });
});
