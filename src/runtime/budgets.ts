import type { Pool } from "pg";

// Резервирование бюджета в базе (§16): уникальность (operation_id, day)
// делает повторное резервирование той же операции невозможным —
// перезапуск процесса не размножает лимиты.
export interface Budget {
  /** true — резерв получен; false — такая операция уже резервировалась сегодня. */
  tryReserve(operationId: string): Promise<boolean>;
  /** Сколько операций зарезервировано сегодня (опционально — только с таким префиксом). */
  reservedToday(prefix?: string): Promise<number>;
}

export class BudgetRepo implements Budget {
  constructor(private readonly pool: Pool) {}

  async tryReserve(operationId: string): Promise<boolean> {
    const result = await this.pool.query(
      `INSERT INTO budget_reservations (operation_id, day)
       VALUES ($1, CURRENT_DATE)
       ON CONFLICT (operation_id, day) DO NOTHING
       RETURNING id`,
      [operationId],
    );
    return (result.rowCount ?? 0) > 0;
  }

  async reservedToday(prefix?: string): Promise<number> {
    const result = await this.pool.query<{ count: string }>(
      `SELECT count(*)::text AS count FROM budget_reservations
       WHERE day = CURRENT_DATE AND ($1::text IS NULL OR operation_id LIKE $1 || '%')`,
      [prefix ?? null],
    );
    return Number(result.rows[0]?.["count"] ?? 0);
  }
}
