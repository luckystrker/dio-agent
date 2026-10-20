// Интеграционный тест (живой PostgreSQL, база dio_test).
// Инвариант 4 / §12.1: применение списка дельт «всё или ничего» внутри
// withTransaction — ошибка посреди списка откатывает весь набор.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { applyDeltas, validateDeltas } from "./deltas.js";
import { withTransaction } from "./transactions.js";
import { WorkQueueRepo } from "./work-queue.js";
import { cleanTables, createTestPool } from "../test-support/db.js";
import type { Pool } from "pg";

let pool: Pool;

beforeEach(async () => {
  pool = createTestPool();
  await cleanTables(pool);
});

afterEach(async () => {
  await pool.end();
});

describe("дельты: всё или ничего", () => {
  it("валидный список применяется целиком внутри withTransaction", async () => {
    const repo = new WorkQueueRepo(pool);
    const workId = await repo.enqueue("direct", {});

    const deltas = validateDeltas([
      { type: "append_note", text: "первая заметка" },
      { type: "append_note", text: "вторая заметка" },
      { type: "append_note", text: "третья заметка" },
    ]);

    const applied = await withTransaction(pool, (client) => applyDeltas(client, workId, deltas));
    expect(applied).toBe(3);

    const rows = await pool.query<{ text: string; work_item_id: string }>(
      "SELECT text, work_item_id FROM state_notes ORDER BY id",
    );
    expect(rows.rows.map((row) => row.text)).toEqual(["первая заметка", "вторая заметка", "третья заметка"]);
    expect(rows.rows.every((row) => String(row.work_item_id) === workId)).toBe(true);
  });

  it("ошибка посреди списка откатывает ВСЁ: первая дельта не видна", async () => {
    const repo = new WorkQueueRepo(pool);
    const validWorkId = await repo.enqueue("direct", {});

    await expect(
      withTransaction(pool, async (client) => {
        // Первая часть списка — валидные дельты на существующую работу.
        const firstBatch = await applyDeltas(client, validWorkId, validateDeltas([
          { type: "append_note", text: "дельта до сбоя — её быть не должно" },
          { type: "append_note", text: "ещё одна до сбоя" },
        ]));
        // Посреди применения — сбой: FK на несуществующий work_item_id.
        const nonExistentWorkId = "999999999999";
        await applyDeltas(
          client,
          nonExistentWorkId,
          validateDeltas([{ type: "append_note", text: "дельта со сбойного места" }]),
        );
        return firstBatch;
      }),
    ).rejects.toThrow();

    const remaining = await pool.query("SELECT count(*)::int AS n FROM state_notes");
    expect(remaining.rows[0]?.n).toBe(0); // откатилось всё, включая первую дельту
  });

  it("ошибка в самом конце (после дельт) тоже откатывает весь набор", async () => {
    const repo = new WorkQueueRepo(pool);
    const workId = await repo.enqueue("group", {});

    await expect(
      withTransaction(pool, async (client) => {
        await applyDeltas(client, workId, validateDeltas([{ type: "append_note", text: "заметка" }]));
        await client.query("SELECT 1/0"); // сбой уже после применения дельт
      }),
    ).rejects.toThrow();

    const remaining = await pool.query("SELECT count(*)::int AS n FROM state_notes");
    expect(remaining.rows[0]?.n).toBe(0);
  });

  it("withTransaction возвращает результат fn и фиксирует его коммитом", async () => {
    const repo = new WorkQueueRepo(pool);
    const workId = await repo.enqueue("intention", {});

    const marker = await withTransaction(pool, async (client) => {
      await applyDeltas(client, workId, validateDeltas([{ type: "append_note", text: "фиксация" }]));
      return "committed-marker";
    });
    expect(marker).toBe("committed-marker");

    const rows = await pool.query("SELECT count(*)::int AS n FROM state_notes");
    expect(rows.rows[0]?.n).toBe(1);
  });

  it("applyDeltas с workItemId=null пишет заметку без привязки к работе", async () => {
    const applied = await withTransaction(pool, (client) =>
      applyDeltas(client, null, [{ type: "append_note", text: "заметка без работы" }]),
    );
    expect(applied).toBe(1);
    const rows = await pool.query<{ work_item_id: string | null; text: string }>(
      "SELECT work_item_id, text FROM state_notes",
    );
    expect(rows.rows).toHaveLength(1);
    expect(rows.rows[0]?.work_item_id).toBeNull();
    expect(rows.rows[0]?.text).toBe("заметка без работы");
  });

  it("после отката соединение возвращается в пул и работоспособно", async () => {
    const repo = new WorkQueueRepo(pool);
    const workId = await repo.enqueue("exploration", {});

    await expect(
      withTransaction(pool, (client) => applyDeltas(client, "999999999999", [
        { type: "append_note", text: "невалидный FK" },
      ])),
    ).rejects.toThrow();

    // Пул не «протекает»: новая транзакция работает на том же пуле.
    await withTransaction(pool, (client) => applyDeltas(client, workId, [
      { type: "append_note", text: "после отката" },
    ]));
    const rows = await pool.query("SELECT count(*)::int AS n FROM state_notes");
    expect(rows.rows[0]?.n).toBe(1);
  });
});
