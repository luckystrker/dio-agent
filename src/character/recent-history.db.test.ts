import { afterAll, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { recentHistory } from "./recent-history.js";
import { createTestPool } from "../test-support/db.js";

// 8.1: история беседы = входящие события журнала + отправленные ответы,
// по времени; unsent/pending/unknown в разговор не попадают (§13.5).

const pool: Pool = await createTestPool();

let eventId = 9_000;
let turnId = 9_000;

async function insertUserMessage(chatId: number, text: string, minutesAgo: number): Promise<void> {
  await pool.query(
    `INSERT INTO social_events (telegram_update_id, chat_id, chat_type, kind, text, status, raw_update, occurred_at)
     VALUES ($1, $2, 'private', 'message', $3, 'processed', '{}', now() - ($4 || ' minutes')::interval)`,
    [eventId++, chatId, text, minutesAgo],
  );
}

async function insertSentReply(chatId: number, text: string, minutesAgo: number): Promise<void> {
  const turn = await pool.query<{ id: string }>(
    "INSERT INTO turns (chat_id) VALUES ($1) RETURNING id",
    [chatId],
  );
  await pool.query(
    `INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, created_at)
     VALUES ($1, 0, $2, $3::jsonb, 'sent', now() - ($4 || ' minutes')::interval)`,
    [turn.rows[0]!["id"], chatId, JSON.stringify({ text }), minutesAgo],
  );
}

async function insertUnsentReply(chatId: number, text: string): Promise<void> {
  const turn = await pool.query<{ id: string }>(
    "INSERT INTO turns (chat_id) VALUES ($1) RETURNING id",
    [turnId++],
  );
  await pool.query(
    `INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status)
     VALUES ($1, 0, $2, $3::jsonb, 'pending')`,
    [turn.rows[0]!["id"], chatId, JSON.stringify({ text })],
  );
}

beforeEach(async () => {
  await pool.query("DELETE FROM delivery_outbox");
  await pool.query("DELETE FROM turns");
  await pool.query("DELETE FROM social_events");
});

describe("recentHistory (8.1: недавний разговор в контексте)", () => {
  it("сливает обе стороны по времени: вопрос → ответ → вопрос", async () => {
    await insertUserMessage(707, "старый вопрос", 30);
    await insertSentReply(707, "старый ответ", 29);
    await insertUserMessage(707, "новый вопрос", 1);

    expect(await recentHistory(pool, "707", 100)).toEqual([
      { role: "user", content: "старый вопрос" },
      { role: "assistant", content: "старый ответ" },
      { role: "user", content: "новый вопрос" },
    ]);
  });

  it("берутся последние limit сообщений, более старые не попадают", async () => {
    for (let i = 0; i < 5; i++) await insertUserMessage(708, `сообщение ${i}`, 50 - i);
    const history = await recentHistory(pool, "708", 2);
    expect(history).toEqual([
      { role: "user", content: "сообщение 3" },
      { role: "user", content: "сообщение 4" },
    ]);
  });

  it("неотправленные и неуспешные ответы в разговор не попадают (§13.5)", async () => {
    await insertUserMessage(709, "вопрос", 10);
    await insertSentReply(709, "доставленный ответ", 9);
    await insertUnsentReply(709, "неотправленный черновик");

    expect(await recentHistory(pool, "709", 100)).toEqual([{ role: "user", content: "вопрос" }, { role: "assistant", content: "доставленный ответ" }]);
  });

  it("чаты не смешиваются", async () => {
    await insertUserMessage(710, "из другого чата", 5);
    await insertUserMessage(711, "свой чат", 4);
    expect(await recentHistory(pool, "711", 100)).toEqual([{ role: "user", content: "свой чат" }]);
  });

  it("команды и прочий шум без текста не попадают", async () => {
    await pool.query(
      `INSERT INTO social_events (telegram_update_id, chat_id, chat_type, kind, text, status, raw_update)
       VALUES ($1, 712, 'private', 'message', NULL, 'processed', '{}')`,
      [eventId++],
    );
    await insertUserMessage(712, "текст есть", 5);
    expect(await recentHistory(pool, "712", 100)).toEqual([{ role: "user", content: "текст есть" }]);
  });
});

afterAll(async () => {
  await pool.end();
});
