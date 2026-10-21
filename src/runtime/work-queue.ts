import type { Pool, PoolClient } from "pg";
import { WORK_MAX_ATTEMPTS, WORK_PRIORITIES, type WorkKind } from "./priorities.js";

export type WorkStatus = "pending" | "running" | "done" | "skipped";

export interface WorkItem {
  id: string;
  kind: string;
  priority: number;
  payload: unknown;
  status: WorkStatus;
  attempts: number;
}

/**
 * Чистая часть recovery: что делать с работой, застрявшей в running
 * после падения процесса. Попытка считается в момент claim, поэтому
 * attempts уже включает сорвавшуюся попытку.
 */
export function decideRecoveryAction(
  attempts: number,
  maxAttempts: number = WORK_MAX_ATTEMPTS,
): "requeue" | "skip" {
  return attempts >= maxAttempts ? "skip" : "requeue";
}

interface WorkItemRow {
  id: string | bigint | number;
  kind: string;
  priority: number;
  payload: unknown;
  status: string;
  attempts: number;
}

function toWorkItem(row: WorkItemRow): WorkItem {
  return {
    id: String(row.id),
    kind: row.kind,
    priority: row.priority,
    payload: row.payload,
    status: row.status as WorkStatus,
    attempts: row.attempts,
  };
}

const CLAIM_SQL = `
  UPDATE work_items
  SET status = 'running', attempts = attempts + 1, updated_at = now()
  WHERE id = (
    SELECT id FROM work_items
    WHERE status = 'pending'
      AND (next_attempt_at IS NULL OR next_attempt_at <= now())
    ORDER BY priority DESC, created_at ASC, id ASC
    LIMIT 1
    FOR UPDATE SKIP LOCKED
  )
  RETURNING id, kind, priority, payload, status, attempts
`;

/** Постановка работы на конкретном соединении — внутри чужой транзакции. */
export async function enqueueWithClient(
  client: PoolClient,
  kind: WorkKind,
  payload: unknown,
  priority?: number,
): Promise<string> {
  const resolved = priority ?? WORK_PRIORITIES[kind];
  const result = await client.query<{ id: string | bigint }>(
    "INSERT INTO work_items (kind, priority, payload) VALUES ($1, $2, $3) RETURNING id",
    [kind, resolved, JSON.stringify(payload ?? {})],
  );
  return String(result.rows[0]?.id ?? "");
}

export class WorkQueueRepo {
  constructor(private readonly pool: Pool) {}

  async enqueue(kind: WorkKind, payload: unknown, priority?: number): Promise<string> {
    const resolved = priority ?? WORK_PRIORITIES[kind];
    const result = await this.pool
      .query<{ id: string | bigint }>(
        "INSERT INTO work_items (kind, priority, payload) VALUES ($1, $2, $3) RETURNING id",
        [kind, resolved, JSON.stringify(payload ?? {})],
      );
    return String(result.rows[0]?.id ?? "");
  }

  /** Захватить самую приоритетную pending-работу (FIFO внутри приоритета). */
  async claimNext(): Promise<WorkItem | null> {
    const result = await this.pool.query<WorkItemRow>(CLAIM_SQL);
    const row = result.rows[0];
    return row ? toWorkItem(row) : null;
  }

  async complete(id: string): Promise<void> {
    await this.pool.query("UPDATE work_items SET status = 'done', updated_at = now() WHERE id = $1", [id]);
  }

  /** Вернуть в очередь; pauseMs — пауза до следующей попытки (§12.1). */
  async requeue(id: string, pauseMs = 0): Promise<void> {
    await this.pool.query(
      `UPDATE work_items
       SET status = 'pending', next_attempt_at = now() + ($2::bigint * interval '1 millisecond'), updated_at = now()
       WHERE id = $1`,
      [id, Math.max(0, Math.round(pauseMs))],
    );
  }

  /** Терминальный пропуск: работа сознательно не будет обработана, и это видно. */
  async skip(id: string, reason: string): Promise<void> {
    await this.pool.query(
      "UPDATE work_items SET status = 'skipped', skip_reason = $2, updated_at = now() WHERE id = $1",
      [id, reason],
    );
  }

  /**
   * Recovery после жёсткого падения (вплоть до SIGKILL): всё, что висит
   * в running, либо возвращается в pending, либо помечается пропуском —
   * «вечно в работе» не остаётся ничего (ARCHITECTURE §5.3).
   */
  async recoverStale(): Promise<{ requeued: number; skipped: number }> {
    return this.inTransaction(async (client) => {
      const stale = await client.query<WorkItemRow>(
        "SELECT id, kind, priority, payload, status, attempts FROM work_items WHERE status = 'running' ORDER BY id",
      );
      let requeued = 0;
      let skipped = 0;
      for (const row of stale.rows) {
        const item = toWorkItem(row);
        if (decideRecoveryAction(item.attempts) === "skip") {
          await client.query(
            "UPDATE work_items SET status = 'skipped', skip_reason = $2, updated_at = now() WHERE id = $1",
            [item.id, "превышен лимит попыток после сбоя"],
          );
          skipped++;
        } else {
          await client.query(
            "UPDATE work_items SET status = 'pending', updated_at = now() WHERE id = $1",
            [item.id],
          );
          requeued++;
        }
      }
      return { requeued, skipped };
    });
  }

  async pendingCount(): Promise<number> {
    const result = await this.pool.query<{ count: string }>(
      "SELECT count(*)::text AS count FROM work_items WHERE status = 'pending'",
    );
    return Number(result.rows[0]?.["count"] ?? 0);
  }

  private inTransaction<T>(fn: (client: PoolClient) => Promise<T>): Promise<T> {
    return this.pool.connect().then(async (client) => {
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
    });
  }
}
