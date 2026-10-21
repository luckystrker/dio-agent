import type { Pool, PoolClient } from "pg";
import { normalizeUpdate, type NormalizedEvent } from "./normalize.js";
import { decideIngestStatus } from "./prefilter.js";

// Короткая транзакция ingest (§6.1): событие и следующий offset — атомарно.
// Offset двигается ТОЛЬКО здесь, и только после надёжной записи события
// (или подтверждения, что оно уже сохранено). Ingest ничего не ждёт:
// ни модели, ни буфера, ни доставки (инвариант 1).

export const POLL_STATE_OFFSET_KEY = "telegram_offset";

export interface IngestResult {
  /** false — событие уже было сохранено ранее (дубль пачки), это не ошибка */
  inserted: boolean;
  /** id строки social_events (для новой записи) */
  eventId: string | null;
  /** решение prefilter для новой записи */
  status: "pending" | "journal_only";
}

export async function ingestUpdate(
  pool: Pool,
  rawUpdate: unknown,
): Promise<IngestResult & { event: NormalizedEvent }> {
  const event = normalizeUpdate(rawUpdate);
  const status = decideIngestStatus(event);

  return withClient(pool, async (client) => {
    await client.query("BEGIN");
    try {
      const inserted = await client.query<{ id: string | bigint }>(
        `INSERT INTO social_events
           (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
         ON CONFLICT (telegram_update_id) DO NOTHING
         RETURNING id`,
        [
          event.telegramUpdateId,
          event.chatId,
          event.chatType,
          event.actorId,
          event.actorName,
          event.kind,
          event.text,
          JSON.stringify(rawUpdate),
          status,
        ],
      );
      // Тот же offset в той же транзакции (§6.1, шаг 5)
      await client.query(
        `INSERT INTO poll_state (key, value) VALUES ($1, $2)
         ON CONFLICT (key) DO UPDATE SET value = GREATEST(poll_state.value, EXCLUDED.value), updated_at = now()`,
        [POLL_STATE_OFFSET_KEY, String(Number(event.telegramUpdateId) + 1)],
      );
      await client.query("COMMIT");

      const row = inserted.rows[0];
      return {
        inserted: row !== undefined,
        eventId: row ? String(row.id) : null,
        status,
        event,
      };
    } catch (error) {
      await client.query("ROLLBACK").catch(() => undefined);
      throw error;
    }
  });
}

export async function readOffset(pool: Pool): Promise<number | null> {
  const result = await pool.query<{ value: string }>(
    "SELECT value::text FROM poll_state WHERE key = $1",
    [POLL_STATE_OFFSET_KEY],
  );
  const raw = result.rows[0]?.["value"];
  return raw === undefined ? null : Number(raw);
}

async function withClient<T>(pool: Pool, fn: (client: PoolClient) => Promise<T>): Promise<T> {
  const client = await pool.connect();
  try {
    return await fn(client);
  } finally {
    client.release();
  }
}
