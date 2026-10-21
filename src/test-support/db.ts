// Общие помощники интеграционных тестов (живой PostgreSQL, база dio_test).

import { Pool } from "pg";
import { createDatabasePool } from "../runtime/db.js";

/** Отдельная база для тестов; разработческая dio не используется. */
export const TEST_DATABASE_URL =
  process.env["TEST_DATABASE_URL"] ?? "postgres://dio:dio@127.0.0.1:5433/dio_test";

export function createTestPool(): Pool {
  return createDatabasePool(TEST_DATABASE_URL);
}

/** Полная очистка таблиц тестов между сценариями. */
export async function cleanTables(pool: Pool): Promise<void> {
  await pool.query(
    "TRUNCATE TABLE state_notes, work_items, budget_reservations RESTART IDENTITY CASCADE",
  );
}

/** Очистка таблиц памяти (этап 3): entities, memory_items, links, sources. */
export async function cleanMemoryTables(pool: Pool): Promise<void> {
  await pool.query(
    "TRUNCATE TABLE stance_basis, stances, memory_derivations, memory_sources, " +
      "memory_links, memory_items, entity_aliases, entities, sources RESTART IDENTITY CASCADE",
  );
}

/** Поллинг условия с дедлайном — для ожидания асинхронного worker'а. */
export async function waitFor(
  condition: () => Promise<boolean>,
  options: { timeoutMs?: number; label?: string } = {},
): Promise<void> {
  const timeoutMs = options.timeoutMs ?? 10_000;
  const label = options.label ?? "условие";
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await condition()) return;
    await sleep(20);
  }
  throw new Error(`waitFor: истёк таймаут ${timeoutMs} мс — ${label}`);
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export interface WorkItemRow {
  id: string;
  kind: string;
  priority: number;
  status: string;
  attempts: number;
  skip_reason: string | null;
}

export async function getWorkItem(pool: Pool, id: string): Promise<WorkItemRow | undefined> {
  const result = await pool.query<WorkItemRow>(
    "SELECT id, kind, priority, status, attempts, skip_reason FROM work_items WHERE id = $1",
    [id],
  );
  return result.rows[0];
}
