import type { Pool, PoolClient } from "pg";

// Транзакционная обёртка: «всё или ничего» (ARCHITECTURE §5, инвариант 4).
// ROLLBACK при любой ошибке fn; соединение всегда возвращается в пул.
export async function withTransaction<T>(
  pool: Pool,
  fn: (client: PoolClient) => Promise<T>,
): Promise<T> {
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    const result = await fn(client);
    await client.query("COMMIT");
    return result;
  } catch (error) {
    await client.query("ROLLBACK").catch(() => undefined);
    throw error;
  } finally {
    client.release();
  }
}
