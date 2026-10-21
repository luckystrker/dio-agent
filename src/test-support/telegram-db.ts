// Помощники DB-тестов Telegram-слоя (живой PostgreSQL, база dio_test).
// Дополняют db.ts: чистят таблицы этапа 2 и вставляют события напрямую.

import type { Pool } from "pg";
import type { Logger } from "../runtime/logger.js";

/** Молчаливый логгер: циклы polling/delivery в тестах не должны шуметь. */
export const silentLogger: Logger = {
  debug: () => undefined,
  info: () => undefined,
  warn: () => undefined,
  error: () => undefined,
};

/** Полная очистка таблиц этапа 2 (+ общих) между сценариями. */
export async function cleanTelegramTables(pool: Pool): Promise<void> {
  await pool.query(
    "TRUNCATE TABLE delivery_outbox, turns, social_events, poll_state, state_notes, work_items, budget_reservations RESTART IDENTITY CASCADE",
  );
}

export interface InsertEventOptions {
  telegramUpdateId: number;
  chatId: string;
  chatType: "private" | "group";
  kind?: string;
  text?: string | null;
  actorId?: string | null;
  /** по умолчанию pending — как после ingest без буферизации */
  status?: string;
}

/** Прямая вставка social_events — как это сделал бы ingest. */
export async function insertEvent(pool: Pool, options: InsertEventOptions): Promise<string> {
  const result = await pool.query<{ id: string | bigint }>(
    `INSERT INTO social_events
       (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status)
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
     RETURNING id`,
    [
      options.telegramUpdateId,
      options.chatId,
      options.chatType,
      options.actorId ?? "1",
      options.kind ?? "message",
      options.text ?? null,
      JSON.stringify({ update_id: options.telegramUpdateId, stub: true }),
      options.status ?? "pending",
    ],
  );
  return String(result.rows[0]!.id);
}

export async function createTurnRow(pool: Pool, chatId: string): Promise<string> {
  const result = await pool.query<{ id: string | bigint }>(
    "INSERT INTO turns (chat_id) VALUES ($1) RETURNING id",
    [chatId],
  );
  return String(result.rows[0]!.id);
}

export interface OutboxDbRow {
  id: string;
  turn_id: string;
  position: number;
  chat_id: string;
  payload: { text?: string };
  status: string;
  attempts: number;
  telegram_message_id: string | null;
  last_error: string | null;
}

export async function insertOutboxRow(
  pool: Pool,
  options: {
    turnId: string;
    chatId: string;
    position: number;
    text: string;
    status?: string;
    attempts?: number;
    notBeforeSql?: string;
  },
): Promise<string> {
  const result = await pool.query<{ id: string | bigint }>(
    `INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, attempts, not_before)
     VALUES ($1, $2, $3, $4, $5, $6, ${options.notBeforeSql ?? "now()"})
     RETURNING id`,
    [
      options.turnId,
      options.position,
      options.chatId,
      JSON.stringify({ text: options.text }),
      options.status ?? "pending",
      options.attempts ?? 0,
    ],
  );
  return String(result.rows[0]!.id);
}

export async function getOutboxByTurn(pool: Pool, turnId: string): Promise<OutboxDbRow[]> {
  const result = await pool.query<OutboxDbRow>(
    "SELECT id::text, turn_id::text, position, chat_id::text, payload, status, attempts, telegram_message_id::text, last_error FROM delivery_outbox WHERE turn_id = $1 ORDER BY position",
    [turnId],
  );
  return result.rows;
}

export async function getOutboxRow(pool: Pool, id: string): Promise<OutboxDbRow | undefined> {
  const result = await pool.query<OutboxDbRow>(
    "SELECT id::text, turn_id::text, position, chat_id::text, payload, status, attempts, telegram_message_id::text, last_error FROM delivery_outbox WHERE id = $1",
    [id],
  );
  return result.rows[0];
}

/**
 * Ожидание реального (нестрогого к фейковым таймерам) условия: крутит
 * setImmediate, не трогая setTimeout/Date — сочетается с
 * vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] }).
 */
export async function waitForDb(
  condition: () => Promise<boolean>,
  label: string,
  timeoutMs = 5_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (await condition()) return;
    if (Date.now() > deadline) {
      throw new Error(`waitForDb: истёк таймаут ${timeoutMs} мс — ${label}`);
    }
    await new Promise((resolve) => setImmediate(resolve));
  }
}
