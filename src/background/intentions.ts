import type { Pool, PoolClient } from "pg";
import { enqueueWithClient } from "../runtime/work-queue.js";
import type { IntentionKind } from "./intention-proposals.js";
import { INTENTION_TTL_HOURS, intentionDedupeKey } from "./intention-proposals.js";

// Intentions (§14.1): единый механизм отложенных действий. Dedupe по
// (kind, topic, chat); сроки notBefore/expiresAt; переход в работу —
// транзакционно, повторная постановка невозможна.

export type IntentionStatus = "pending" | "queued" | "running" | "completed" | "cancelled" | "failed";

export interface IntentionRow {
  id: string;
  kind: IntentionKind;
  targetChatId: string;
  topic: string;
  reason: string;
  status: IntentionStatus;
  notBefore: Date;
  expiresAt: Date | null;
  relatedTurnId: string | null;
  createdAt: Date;
}

interface IntentionDbRow {
  id: string | bigint;
  kind: string;
  target_chat_id: string | bigint;
  topic: string;
  reason: string;
  status: string;
  not_before: Date;
  expires_at: Date | null;
  related_turn_id: string | bigint | null;
  created_at: Date;
}

function toRow(row: IntentionDbRow): IntentionRow {
  return {
    id: String(row.id),
    kind: row.kind as IntentionKind,
    targetChatId: String(row.target_chat_id),
    topic: row.topic,
    reason: row.reason,
    status: row.status as IntentionStatus,
    notBefore: row.not_before,
    expiresAt: row.expires_at,
    relatedTurnId: row.related_turn_id === null ? null : String(row.related_turn_id),
    createdAt: row.created_at,
  };
}

export interface CreateIntentionInput {
  kind: IntentionKind;
  topic: string;
  reason: string;
  targetChatId: string;
  delayHours: number;
  relatedTurnId?: string | null;
}

export class IntentionRepo {
  constructor(private readonly pool: Pool) {}

  /**
   * Создать намерение с dedupe: повтор той же идеи (kind+topic+chat)
   * не создаёт второе. Возвращает созданную строку или null при дубликате.
   */
  async create(client: PoolClient, input: CreateIntentionInput): Promise<IntentionRow | null> {
    const notBefore = new Date(Date.now() + input.delayHours * 3_600_000);
    const expiresAt = new Date(notBefore.getTime() + INTENTION_TTL_HOURS * 3_600_000);
    const dedupeKey = intentionDedupeKey(input.kind, input.topic, input.targetChatId);

    const inserted = await client.query<IntentionDbRow>(
      `INSERT INTO intentions
         (kind, target_chat_id, topic, reason, not_before, expires_at, dedupe_key, related_turn_id)
       VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
       ON CONFLICT (dedupe_key) DO NOTHING
       RETURNING id, kind, target_chat_id, topic, reason, status, not_before, expires_at, related_turn_id, created_at`,
      [
        input.kind,
        input.targetChatId,
        input.topic,
        input.reason,
        notBefore,
        expiresAt,
        dedupeKey,
        input.relatedTurnId ?? null,
      ],
    );
    return inserted.rows[0] ? toRow(inserted.rows[0]) : null;
  }

  async byId(id: string): Promise<IntentionRow | null> {
    const result = await this.pool.query<IntentionDbRow>(
      `SELECT id, kind, target_chat_id, topic, reason, status, not_before, expires_at, related_turn_id, created_at
       FROM intentions WHERE id = $1`,
      [id],
    );
    return result.rows[0] ? toRow(result.rows[0]) : null;
  }

  /**
   * Планировщик §14.2: due-намерения (срок пришёл, не истёк, ещё pending)
   * атомарно переводятся в queued И ставятся в работу — двойная постановка
   * невозможна (CAS статуса в той же транзакции).
   */
  async claimDue(limit = 5): Promise<number> {
    const client = await this.pool.connect();
    try {
      await client.query("BEGIN");
      const due = await client.query<IntentionDbRow>(
        `UPDATE intentions SET status = 'queued', updated_at = now()
         WHERE id IN (
           SELECT id FROM intentions
           WHERE status = 'pending' AND not_before <= now()
             AND (expires_at IS NULL OR expires_at > now())
           ORDER BY not_before
           LIMIT $1
           FOR UPDATE SKIP LOCKED
         )
         RETURNING id, kind, target_chat_id, topic, reason, status, not_before, expires_at, related_turn_id, created_at`,
        [limit],
      );
      for (const row of due.rows) {
        await enqueueWithClient(client, "intention", { intentionId: String(row.id) });
      }
      await client.query("COMMIT");
      return due.rows.length;
    } catch (error) {
      await client.query("ROLLBACK").catch(() => undefined);
      throw error;
    } finally {
      client.release();
    }
  }

  async markRunning(id: string): Promise<void> {
    await this.pool.query(
      "UPDATE intentions SET status = 'running', updated_at = now() WHERE id = $1 AND status = 'queued'",
      [id],
    );
  }

  async complete(id: string): Promise<void> {
    await this.pool.query(
      "UPDATE intentions SET status = 'completed', updated_at = now() WHERE id = $1",
      [id],
    );
  }

  async cancel(id: string, reason: string): Promise<void> {
    await this.pool.query(
      "UPDATE intentions SET status = 'cancelled', reason = $2, updated_at = now() WHERE id = $1",
      [id, `отменено: ${reason}`],
    );
  }

  /** Сколько explore-намерений завершено сегодня (§16: до 2 в сутки). */
  async exploreCompletedToday(): Promise<number> {
    const result = await this.pool.query<{ count: string }>(
      `SELECT count(*)::text AS count FROM intentions
       WHERE kind = 'explore' AND status = 'completed' AND updated_at >= date_trunc('day', now())`,
    );
    return Number(result.rows[0]?.["count"] ?? 0);
  }

  /**
   * Проактивная политика §14.3: ≤2 инициативных отправок на личный чат
   * в сутки, ≥4 часа между ними; группы выключены (isPrivate).
   */
  async proactiveAllowed(chatId: string, isPrivate: boolean): Promise<boolean> {
    if (!isPrivate) return false;
    const result = await this.pool.query<{ last: Date | null; today: string }>(
      `SELECT max(sent_at) AS last, count(*) FILTER (WHERE sent_at >= date_trunc('day', now()))::text AS today
       FROM proactive_log WHERE chat_id = $1`,
      [chatId],
    );
    const row = result.rows[0];
    if (!row) return true;
    if (Number(row.today) >= 2) return false;
    if (row.last !== null && Date.now() - row.last.getTime() < 4 * 3_600_000) return false;
    return true;
  }

  /** Зафиксировать инициативную отправку (§14.3). */
  async logProactive(client: PoolClient, chatId: string, intentionId: string): Promise<void> {
    await client.query(
      "INSERT INTO proactive_log (chat_id, intention_id) VALUES ($1, $2)",
      [chatId, intentionId],
    );
  }
}
