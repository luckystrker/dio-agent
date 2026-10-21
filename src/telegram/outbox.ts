import type { Pool, PoolClient } from "pg";

// Outbox (§13.1): единицы доставки со статусами pending | sending | sent |
// retry_wait | unknown | cancelled | failed. UNIQUE(turn_id, position) —
// внутренний ключ против дублей намерений. Внутренние ключи не обещают
// exactly-once во внешнем API (инвариант 12).

export type OutboxStatus =
  | "pending"
  | "sending"
  | "sent"
  | "retry_wait"
  | "unknown"
  | "cancelled"
  | "failed";

export interface OutboxRow {
  id: string;
  turnId: string;
  position: number;
  chatId: string;
  kind: "message" | "reaction";
  text: string;
  emoji: string | null;
  targetMessageId: string | null;
  status: OutboxStatus;
  attempts: number;
}

interface OutboxRowDb {
  id: string | bigint;
  turn_id: string | bigint;
  position: number;
  chat_id: string | bigint;
  payload: { text?: string; type?: string; emoji?: string; message_id?: string | number };
  status: string;
  attempts: number;
}

function toRow(row: OutboxRowDb): OutboxRow {
  const isReaction = row.payload?.["type"] === "reaction";
  return {
    id: String(row.id),
    turnId: String(row.turn_id),
    position: row.position,
    chatId: String(row.chat_id),
    kind: isReaction ? "reaction" : "message",
    text: row.payload?.["text"] ?? "",
    emoji: isReaction ? (row.payload?.["emoji"] ?? null) : null,
    targetMessageId: isReaction
      ? row.payload?.["message_id"] !== undefined
        ? String(row.payload["message_id"])
        : null
      : null,
    status: row.status as OutboxStatus,
    attempts: row.attempts,
  };
}

const DELIVERABLE_STATUSES = "'pending', 'retry_wait', 'sending'";

function compareDeliveryOrder(a: OutboxRow, b: OutboxRow): number {
  const byChat = Number(a.chatId) - Number(b.chatId);
  if (byChat !== 0) return byChat;
  const byTurn = Number(a.turnId) - Number(b.turnId);
  if (byTurn !== 0) return byTurn;
  return a.position - b.position;
}

export class OutboxRepo {
  constructor(private readonly pool: Pool) {}

  /**
   * Создание строк доставки ВНУТРИ транзакции применения хода.
   * Идемпотентно: конфликт (turn_id, position) молча пропускается —
   * повторное применение ничего не меняет (§12.1).
   * not_before размечает интервалы между пузырями (§13.3: 300–1500 мс).
   */
  async enqueueMessages(
    client: PoolClient,
    turnId: string,
    chatId: string,
    texts: string[],
    bubbleDelayMs = 800,
  ): Promise<number> {
    if (texts.length === 0) return 0;
    const fragments: string[] = [];
    const params: unknown[] = [];
    texts.forEach((text, position) => {
      const base = 4 + position * 4;
      fragments.push(
        `($${base - 3}, $${base - 2}, $${base - 1}, $${base}, now() + ($${texts.length * 4 + 1} * ${position}::bigint * interval '1 millisecond'))`,
      );
      params.push(turnId, position, chatId, JSON.stringify({ text }));
    });
    params.push(bubbleDelayMs);
    const inserted = await client.query<{ id: string }>(
      `INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, not_before)
       VALUES ${fragments.join(", ")}
       ON CONFLICT (turn_id, position) DO NOTHING
       RETURNING id`,
      params,
    );
    return inserted.rowCount ?? 0;
  }

  /**
   * Реакция на сообщение — единица доставки с UNIQUE(turn_id, position).
   * Реакция — установка состояния: своя retry-политика у адаптера (§13.2).
   */
  async enqueueReaction(
    client: PoolClient,
    turnId: string,
    chatId: string,
    emoji: string,
    targetMessageId: string,
  ): Promise<number> {
    const inserted = await client.query<{ id: string }>(
      `INSERT INTO delivery_outbox (turn_id, position, chat_id, payload)
       VALUES ($1, 0, $2, $3::jsonb)
       ON CONFLICT (turn_id, position) DO NOTHING
       RETURNING id`,
      [turnId, chatId, JSON.stringify({ type: "reaction", emoji, message_id: targetMessageId })],
    );
    return inserted.rowCount ?? 0;
  }

  /** Захват к отправке: pending/retry_wait со наступившим not_before, по порядку чата. */
  async claimDue(limit: number): Promise<OutboxRow[]> {
    const client = await this.pool.connect();
    try {
      await client.query("BEGIN");
      const rows = await client.query<OutboxRowDb>(
        `SELECT id, turn_id, position, chat_id, payload, status, attempts
         FROM delivery_outbox
         WHERE (status = 'pending' OR (status = 'retry_wait' AND not_before <= now()))
           AND not_before <= now()
         ORDER BY chat_id ASC, turn_id ASC, position ASC
         LIMIT $1
         FOR UPDATE SKIP LOCKED`,
        [limit],
      );
      if (rows.rows.length === 0) {
        await client.query("COMMIT");
        return [];
      }
      const ids = rows.rows.map((row) => BigInt(row.id));
      const claimed = await client.query<OutboxRowDb>(
        `UPDATE delivery_outbox
         SET status = 'sending', attempts = attempts + 1, updated_at = now()
         WHERE id = ANY($1::bigint[])
         RETURNING id, turn_id, position, chat_id, payload, status, attempts`,
        [ids.map(String)],
      );
      await client.query("COMMIT");
      // §13.1: порядок внутри чата обязателен; RETURNING отдаёт физический
      // порядок вставки — пересортируем явно (находка Н1 приёмки этапа 2)
      return claimed.rows.map(toRow).sort(compareDeliveryOrder);
    } catch (error) {
      await client.query("ROLLBACK").catch(() => undefined);
      throw error;
    } finally {
      client.release();
    }
  }

  async markSent(id: string, telegramMessageId: string | null): Promise<void> {
    // Условная запись: отменённая (cancelled) строка не перезаписывается
    // отправкой из устаревшей памяти (находка Н2 приёмки этапа 2).
    // null — у реакции нет telegram_message_id (§13.1).
    await this.pool.query(
      `UPDATE delivery_outbox
       SET status = 'sent', telegram_message_id = $2, last_error = NULL, updated_at = now()
       WHERE id = $1 AND status = 'sending'`,
      [id, telegramMessageId],
    );
  }

  /** Текущий статус строки — перепроверка перед отправкой захваченной пачки. */
  async statusOf(id: string): Promise<OutboxStatus | null> {
    const result = await this.pool.query<{ status: string }>(
      "SELECT status FROM delivery_outbox WHERE id = $1",
      [id],
    );
    return (result.rows[0]?.["status"] as OutboxStatus | undefined) ?? null;
  }

  async markRetryWait(id: string, notBeforeMs: number, error: string): Promise<void> {
    await this.pool.query(
      `UPDATE delivery_outbox
       SET status = 'retry_wait', not_before = now() + ($2::bigint * interval '1 millisecond'),
           last_error = $3, updated_at = now()
       WHERE id = $1`,
      [id, Math.max(0, Math.round(notBeforeMs)), error],
    );
  }

  async markFailed(id: string, error: string): Promise<void> {
    await this.pool.query(
      "UPDATE delivery_outbox SET status = 'failed', last_error = $2, updated_at = now() WHERE id = $1",
      [id, error],
    );
  }

  /**
   * Неопределённая отправка (§13.2): timeout/обрыв после возможной отправки.
   * Автоматически НЕ повторяется; оставшиеся зависимые пузыри
   * последовательности отменяются — компромисс в пользу отсутствия дублей.
   */
  async markUnknown(id: string, error: string): Promise<number> {
    return this.cancelFrom(id, "unknown", error, 0);
  }

  /** Отмена ещё не отправленных пузырей_turn, начиная с позиции position. */
  async cancelTurnFrom(turnId: string, position: number, reason: string): Promise<number> {
    const result = await this.pool.query(
      `UPDATE delivery_outbox
       SET status = 'cancelled', last_error = $3, updated_at = now()
       WHERE turn_id = $1 AND position >= $2 AND status IN (${DELIVERABLE_STATUSES})`,
      [turnId, position, reason],
    );
    return result.rowCount ?? 0;
  }

  /**
   * Recovery (§12.2): sending старого процесса — в unknown, не отправлять
   * вслепую; зависимые отменяются.
   */
  async recoverStaleSending(staleMs = 60_000): Promise<number> {
    const stale = await this.pool.query<{ id: string; turn_id: string; position: number }>(
      `SELECT id::text, turn_id::text, position FROM delivery_outbox
       WHERE status = 'sending' AND updated_at < now() - ($1::bigint * interval '1 millisecond')`,
      [staleMs],
    );
    for (const row of stale.rows) {
      await this.cancelFrom(row.id, "unknown", "sending завис после сбоя процесса", row.position);
    }
    return stale.rows.length;
  }

  private async cancelFrom(
    id: string,
    status: "unknown",
    error: string,
    fromPosition: number,
  ): Promise<number> {
    const client = await this.pool.connect();
    try {
      await client.query("BEGIN");
      const row = await client.query<{ turn_id: string; position: number }>(
        "UPDATE delivery_outbox SET status = $2, last_error = $3, updated_at = now() WHERE id = $1 RETURNING turn_id::text, position",
        [id, status, error],
      );
      let cancelled = 0;
      const info = row.rows[0];
      if (info) {
        const rest = await client.query(
          `UPDATE delivery_outbox SET status = 'cancelled', last_error = $3, updated_at = now()
           WHERE turn_id = $1 AND position > $2 AND status IN (${DELIVERABLE_STATUSES})`,
          [info.turn_id, Math.max(fromPosition, info.position), "отменено из-за unknown в последовательности"],
        );
        cancelled = rest.rowCount ?? 0;
      }
      await client.query("COMMIT");
      return cancelled;
    } catch (error) {
      await client.query("ROLLBACK").catch(() => undefined);
      throw error;
    } finally {
      client.release();
    }
  }
}
