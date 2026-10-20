// Интеграционный тест (живой PostgreSQL, база dio_test).
// §16: резервирование бюджета в базе — уникальность (operation_id, day)
// делает повторное резервирование той же операции невозможным; счётчик
// суток живёт в базе и переживает рестарты процесса.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { BudgetRepo } from "./budgets.js";
import { BackgroundScheduler } from "./scheduler.js";
import { createLogger } from "./logger.js";
import { cleanTables, createTestPool, sleep } from "../test-support/db.js";
import type { Pool } from "pg";

let pool: Pool;
let repo: BudgetRepo;

beforeEach(async () => {
  pool = createTestPool();
  repo = new BudgetRepo(pool);
  await cleanTables(pool);
});

afterEach(async () => {
  await pool.end();
});

describe("BudgetRepo против реальной базы", () => {
  it("tryReserve идемпотентен на один operation_id: true, затем false, счётчик не растёт", async () => {
    expect(await repo.tryReserve("op-1")).toBe(true);
    expect(await repo.tryReserve("op-1")).toBe(false);
    expect(await repo.tryReserve("op-1")).toBe(false); // и ещё раз — стабильно false
    expect(await repo.reservedToday()).toBe(1);
  });

  it("разные operation_id резервируются независимо, reservedToday считает все", async () => {
    expect(await repo.tryReserve("op-a")).toBe(true);
    expect(await repo.tryReserve("op-b")).toBe(true);
    expect(await repo.tryReserve("op-c")).toBe(true);
    expect(await repo.tryReserve("op-b")).toBe(false); // дубль старого не проходит
    expect(await repo.reservedToday()).toBe(3);
  });

  it("вчерашние резервы не считаются сегодняшним лимитом", async () => {
    await pool.query(
      "INSERT INTO budget_reservations (operation_id, day) VALUES ($1, CURRENT_DATE - 1), ($2, CURRENT_DATE - 1)",
      ["yesterday-1", "yesterday-2"],
    );
    expect(await repo.reservedToday()).toBe(0);
    expect(await repo.tryReserve("today-1")).toBe(true);
    expect(await repo.reservedToday()).toBe(1);
  });

  it("тот же operation_id на другой день — это ДРУГАЯ операция (уникальность по паре)", async () => {
    await pool.query("INSERT INTO budget_reservations (operation_id, day) VALUES ($1, CURRENT_DATE - 1)", [
      "recurring-op",
    ]);
    expect(await repo.tryReserve("recurring-op")).toBe(true); // сегодня — впервые
    expect(await repo.reservedToday()).toBe(1);
  });
});

describe("BackgroundScheduler с настоящим BudgetRepo (§16 + §5.3 рестарт)", () => {
  it("дневной лимит в БД: третий тик за день отказывает даже у НОВОГО экземпляра планировщика", async () => {
    const enqueued: string[] = [];
    const mkScheduler = () =>
      new BackgroundScheduler(
        { enqueue: (kind) => Promise.resolve(enqueued.push(kind)).then(() => undefined), logger: createLogger("error"), budget: repo },
        { maxJobsPerDay: 2 },
      );

    const first = mkScheduler();
    await first.tick();
    await sleep(5); // operationId имеет миллисекундную гранулярность — разводим такты
    await first.tick();
    expect(enqueued).toHaveLength(2);

    // «Рестарт процесса»: новый планировщик, тот же BudgetRepo/база —
    // лимит не обнуляется (инвариант 3: база — источник истины).
    await sleep(5);
    const restarted = mkScheduler();
    await restarted.tick();
    expect(enqueued).toHaveLength(2);
    expect(await repo.reservedToday()).toBe(2);
  });

  it("enqueue с реальным BudgetRepo ставит работу и пишет резерв", async () => {
    const scheduler = new BackgroundScheduler(
      { enqueue: () => Promise.resolve(), logger: createLogger("error"), budget: repo },
      { kind: "exploration" },
    );
    await scheduler.tick();
    const rows = await pool.query<{ operation_id: string }>(
      "SELECT operation_id FROM budget_reservations",
    );
    expect(rows.rows).toHaveLength(1);
    expect(rows.rows[0]?.operation_id).toMatch(/^background:exploration:/);
  });
});
