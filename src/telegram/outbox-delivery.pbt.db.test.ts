// PBT доставки outbox (этап 6.3, §13.1–13.2, инвариант 12): model-based тест.
// Генерируется случайная последовательность операций драйвера доставки
// (enqueue / claim / ok / retry / fail / unknown / cancel / recovery после
// «смерти» процесса). Модель повторяет семантику репозитория и предсказывает
// статус и число попыток КАЖДОЙ строки по ключу (turn_id, position) —
// суррогатные id модель не угадывает (конфликтные INSERT расходуют
// последовательность). claimDue никогда не возвращает уже отправленную
// (sent) или отменённую строку — дублей доставки по построению не возникает.

import { beforeEach, afterEach, describe, expect, it } from "vitest";
import fc from "fast-check";
import type { Pool, PoolClient } from "pg";
import { OutboxRepo } from "./outbox.js";
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

type ModelStatus = "pending" | "sending" | "sent" | "retry_wait" | "unknown" | "cancelled" | "failed";

interface ModelRow {
  turnId: number;
  chatId: number;
  position: number;
  status: ModelStatus;
  attempts: number;
}

const keyOf = (row: { turnId: number; position: number }): string => `${row.turnId}:${row.position}`;

const DELIVERABLE: readonly ModelStatus[] = ["pending", "retry_wait", "sending"];

type Command =
  | { op: "enqueue"; turn: number; texts: number }
  | { op: "claim"; limit: number }
  | { op: "ok"; row: number }
  | { op: "retry"; row: number }
  | { op: "fail"; row: number }
  | { op: "unknown"; row: number }
  | { op: "cancel"; turn: number; position: number }
  | { op: "recover" };

const commandArb: fc.Arbitrary<Command> = fc.oneof(
  fc.record({ op: fc.constant("enqueue" as const), turn: fc.nat(50), texts: fc.integer({ min: 1, max: 3 }) }),
  fc.record({ op: fc.constant("claim" as const), limit: fc.integer({ min: 1, max: 5 }) }),
  fc.record({ op: fc.constant("ok" as const), row: fc.nat(1000) }),
  fc.record({ op: fc.constant("retry" as const), row: fc.nat(1000) }),
  fc.record({ op: fc.constant("fail" as const), row: fc.nat(1000) }),
  fc.record({ op: fc.constant("unknown" as const), row: fc.nat(1000) }),
  fc.record({ op: fc.constant("cancel" as const), turn: fc.nat(50), position: fc.nat(3) }),
  fc.record({ op: fc.constant("recover" as const) }),
);

describe("PBT: доставка outbox без дублей (model-based, живая БД)", () => {
  it("любая последовательность операций драйвера: БД == модель, claim не отдаёт отправленное", async () => {
    await fc.assert(
      fc.asyncProperty(fc.array(commandArb, { minLength: 4, maxLength: 16 }), async (commands) => {
        await cleanTelegramTables(pool);
        const repo = new OutboxRepo(pool);
        const client = await pool.connect();
        try {
          const model: ModelRow[] = [];
          const everSent = new Set<string>();
          /** Суррогатный id строки из БД по ключу — модель его не предсказывает. */
          const dbId = async (row: ModelRow): Promise<string> => {
            const found = await pool.query<{ id: string }>(
              "SELECT id::text FROM delivery_outbox WHERE turn_id = $1 AND position = $2",
              [row.turnId, row.position],
            );
            if (found.rows.length === 0) throw new Error(`dbId: строки ${keyOf(row)} нет в БД`);
            return found.rows[0]!.id;
          };

          for (const command of commands) {
            switch (command.op) {
              case "enqueue": {
                const chatId = 500 + (command.turn % 3);
                const texts = Array.from({ length: command.texts }, (_, index) => `текст ${command.turn}-${index}`);
                const inserted = await repo.enqueueMessages(
                  client as PoolClient,
                  String(command.turn),
                  String(chatId),
                  texts,
                  0,
                );
                // идемпотентность ключа (turn_id, position): повтор того же
                // turn дописывает только НОВЫЕ позиции — модель повторяет
                // ровно эту семантику
                const again = await repo.enqueueMessages(
                  client as PoolClient,
                  String(command.turn),
                  String(chatId),
                  texts,
                  0,
                );
                expect(again).toBe(0);
                const fresh = texts.filter(
                  (_, position) =>
                    !model.some((row) => row.turnId === command.turn && row.position === position),
                ).length;
                expect(inserted).toBe(fresh);
                for (let position = 0; position < command.texts; position++) {
                  if (!model.some((row) => row.turnId === command.turn && row.position === position)) {
                    model.push({ turnId: command.turn, chatId, position, status: "pending", attempts: 0 });
                  }
                }
                break;
              }
              case "claim": {
                const claimed = await repo.claimDue(command.limit);
                for (const row of claimed) {
                  const modelRow = model.find(
                    (candidate) => candidate.turnId === Number(row.turnId) && candidate.position === row.position,
                  );
                  expect(modelRow, `claim вернул неизвестную строку ${row.turnId}:${row.position}`).toBeDefined();
                  expect(DELIVERABLE).toContain(modelRow!.status);
                  expect(everSent.has(keyOf(modelRow!)), "claim отдал уже отправленную строку").toBe(false);
                  modelRow!.status = "sending";
                  modelRow!.attempts += 1;
                }
                // порядок внутри захвата: чат → turn → позиция (§13.1)
                const keys = claimed.map(
                  (row) => [Number(row.chatId), Number(row.turnId), row.position] as const,
                );
                const sorted = [...keys].sort((a, b) => a[0] - b[0] || a[1] - b[1] || a[2] - b[2]);
                expect(keys).toEqual(sorted);
                break;
              }
              case "ok": {
                // реальный драйвер метит только СВОЙ незавершённый захват
                // (sending); отправленное/отменённое он трогать не должен
                const target = pick(model, command.row);
                if (!target || target.status !== "sending") break;
                await repo.markSent(await dbId(target), String(target.turnId * 10 + target.position));
                target.status = "sent";
                everSent.add(keyOf(target));
                break;
              }
              case "retry": {
                // реальный драйвер метит только СВОЙ незавершённый захват
                // (sending); отправленное/отменённое он трогать не должен
                const target = pick(model, command.row);
                if (!target || target.status !== "sending") break;
                await repo.markRetryWait(await dbId(target), 0, "ошибка сети");
                target.status = "retry_wait"; // обновление безусловное — как в репо
                break;
              }
              case "fail": {
                // реальный драйвер метит только СВОЙ незавершённый захват
                // (sending); отправленное/отменённое он трогать не должен
                const target = pick(model, command.row);
                if (!target || target.status !== "sending") break;
                await repo.markFailed(await dbId(target), "нет прав в чате");
                target.status = "failed";
                break;
              }
              case "unknown": {
                // реальный драйвер метит только СВОЙ незавершённый захват
                // (sending); отправленное/отменённое он трогать не должен
                const target = pick(model, command.row);
                if (!target || target.status !== "sending") break;
                await repo.markUnknown(await dbId(target), "timeout после возможной отправки");
                applyUnknown(model, target);
                break;
              }
              case "cancel": {
                await repo.cancelTurnFrom(String(command.turn), command.position, "новые сообщения");
                for (const row of model) {
                  if (
                    row.turnId === command.turn &&
                    row.position >= command.position &&
                    DELIVERABLE.includes(row.status)
                  ) {
                    row.status = "cancelled";
                  }
                }
                break;
              }
              case "recover": {
                await sleep(10); // updated_at захваченных строк строго раньше now()
                await repo.recoverStaleSending(0);
                // Репо обрабатывает СНИМОК sending-строк, и цель помечается
                // unknown безусловно: строка, отменённая предыдущей, всё равно
                // становится unknown (не cancelled). Модель повторяет это.
                const stale = model.filter((row) => row.status === "sending");
                for (const row of stale) applyUnknown(model, row);
                break;
              }
            }

            await expectDbMatchesModel(pool, model);
          }

          // отправка не более одного раза на строку за весь прогон
          for (const sentKey of everSent) {
            const row = model.find((candidate) => keyOf(candidate) === sentKey)!;
            expect(row.status).toBe("sent");
          }
        } finally {
          client.release();
        }
      }),
      { numRuns: 15 },
    );
  });
});

function pick(model: ModelRow[], index: number): ModelRow | undefined {
  if (model.length === 0) return undefined;
  return model[index % model.length]!;
}

/** unknown + отмена последующих доставимых позиций того же turn (§13.2). */
function applyUnknown(model: ModelRow[], target: ModelRow): void {
  target.status = "unknown";
  for (const row of model) {
    if (row.turnId === target.turnId && row.position > target.position && DELIVERABLE.includes(row.status)) {
      row.status = "cancelled";
    }
  }
}

async function expectDbMatchesModel(pool: Pool, model: ModelRow[]): Promise<void> {
  const result = await pool.query<{ turn_id: string; position: number; status: string; attempts: number }>(
    "SELECT turn_id::text, position, status, attempts FROM delivery_outbox",
  );
  expect(result.rows.length).toBe(model.length);
  for (const row of result.rows) {
    const modelRow = model.find(
      (candidate) => candidate.turnId === Number(row.turn_id) && candidate.position === row.position,
    );
    expect(modelRow, `строка ${row.turn_id}:${row.position} есть в БД, но нет в модели`).toBeDefined();
    expect(row.status).toBe(modelRow!.status);
    expect(row.attempts).toBe(modelRow!.attempts);
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
