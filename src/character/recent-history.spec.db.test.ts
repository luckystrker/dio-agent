// Приёмочный БД-тест по спецификации 8.1 (§11.1 п.5), не по коду. Углы, не
// покрытые в recent-history.db.test.ts:
// - слияние обеих сторон при ЧЕРЕДОВАНИИ (5 сообщений, ассистент дважды);
// - события текущего хода (turn_id) исключаются по обе стороны: и входящие
//   из журнала, и отправленный ответ — без дублей с пачкой хода;
// - kind='command'/'edited' в разговор не попадают, kind='mention'/'reply'
//   (с текстом) — попадают;
// - входящее со статусом journal_only ПОПАДАЕТ: это тоже разговор;
// - limit=0 даёт пустую историю; большой limit отдаёт всё;
// - чат-изоляция по обеим сторонам (входящие и ответы).

import { afterAll, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { recentHistory } from "./recent-history.js";
import { createTestPool } from "../test-support/db.js";

// 8.1: история хода — последние сообщения обеих сторон чата, по времени.

const pool: Pool = await createTestPool();

let eventId = 30_000;

interface InsertIncomingOptions {
  kind?: string;
  status?: string;
  turnId?: number;
}

async function insertIncoming(
  chatId: number,
  text: string,
  minutesAgo: number,
  options: InsertIncomingOptions = {},
): Promise<void> {
  await pool.query(
    `INSERT INTO social_events (telegram_update_id, chat_id, chat_type, kind, text, status, raw_update, occurred_at, turn_id)
     VALUES ($1, $2, 'private', $3, $4, $5, '{}', now() - ($6 || ' minutes')::interval, $7)`,
    [
      eventId++,
      chatId,
      options.kind ?? "message",
      text,
      options.status ?? "processed",
      String(minutesAgo),
      options.turnId ?? null,
    ],
  );
}

async function createTurn(chatId: number): Promise<number> {
  const turn = await pool.query<{ id: string }>(
    "INSERT INTO turns (chat_id) VALUES ($1) RETURNING id",
    [chatId],
  );
  return Number(turn.rows[0]!["id"]);
}

async function insertSentReply(
  chatId: number,
  text: string,
  minutesAgo: number,
  turnId?: number,
): Promise<void> {
  const id = turnId ?? (await createTurn(chatId));
  await pool.query(
    `INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, created_at)
     VALUES ($1, 0, $2, $3::jsonb, 'sent', now() - ($4 || ' minutes')::interval)`,
    [id, chatId, JSON.stringify({ text }), minutesAgo],
  );
}

beforeEach(async () => {
  await pool.query("DELETE FROM delivery_outbox");
  await pool.query("DELETE FROM turns");
  await pool.query("DELETE FROM social_events");
});

describe("recentHistory (8.1, §11.1 п.5 — приёмка по спеке)", () => {
  it("сливает стороны по времени при чередовании: u → a → u → a → u", async () => {
    await insertIncoming(801, "u1", 30);
    await insertSentReply(801, "a1", 25);
    await insertIncoming(801, "u2", 20);
    await insertSentReply(801, "a2", 5);
    await insertIncoming(801, "u3", 1);

    expect(await recentHistory(pool, "801", 100)).toEqual([
      { role: "user", content: "u1" },
      { role: "assistant", content: "a1" },
      { role: "user", content: "u2" },
      { role: "assistant", content: "a2" },
      { role: "user", content: "u3" },
    ]);
  });

  it("события текущего хода исключаются по обе стороны: ни входящие с turn_id, ни его отправленный ответ", async () => {
    const chatId = 802;
    await insertIncoming(chatId, "старое-1", 60);
    await insertSentReply(chatId, "старый-ответ", 50);
    await insertIncoming(chatId, "старое-2", 40);

    const current = await createTurn(chatId);
    await insertIncoming(chatId, "вопрос-текущего-хода", 3, { turnId: current });
    await insertIncoming(chatId, "ещё-вопрос-текущего-хода", 2, { turnId: current });
    await insertSentReply(chatId, "ответ-текущего-хода", 1, current);

    // с excludeTurnId: только прежний разговор, без дублей с пачкой хода
    expect(await recentHistory(pool, String(chatId), 100, String(current))).toEqual([
      { role: "user", content: "старое-1" },
      { role: "assistant", content: "старый-ответ" },
      { role: "user", content: "старое-2" },
    ]);

    // без excludeTurnId те же события видны (исключает именно параметр)
    expect(await recentHistory(pool, String(chatId), 100)).toHaveLength(6);
  });

  it("kind='command'/'edited' не попадают, kind='mention'/'reply' с текстом попадают", async () => {
    const chatId = 803;
    await insertIncoming(chatId, "/start", 10, { kind: "command" });
    await insertIncoming(chatId, "правка текста", 9, { kind: "edited" });
    await insertIncoming(chatId, "упоминание", 8, { kind: "mention" });
    await insertIncoming(chatId, "ответ на сообщение", 7, { kind: "reply" });
    await insertIncoming(chatId, "обычное сообщение", 6);

    expect(await recentHistory(pool, String(chatId), 100)).toEqual([
      { role: "user", content: "упоминание" },
      { role: "user", content: "ответ на сообщение" },
      { role: "user", content: "обычное сообщение" },
    ]);
  });

  it("входящее со статусом journal_only попадает в разговор", async () => {
    const chatId = 804;
    await insertIncoming(chatId, "мимо хода, но разговор", 10, { status: "journal_only" });
    await insertIncoming(chatId, "обычное", 5);

    expect(await recentHistory(pool, String(chatId), 100)).toEqual([
      { role: "user", content: "мимо хода, но разговор" },
      { role: "user", content: "обычное" },
    ]);
  });

  it("limit=0 — пустая история; большой limit — весь разговор целиком", async () => {
    const chatId = 805;
    await insertIncoming(chatId, "раз", 30);
    await insertSentReply(chatId, "два", 20);
    await insertIncoming(chatId, "три", 10);

    expect(await recentHistory(pool, String(chatId), 0)).toEqual([]);
    expect(await recentHistory(pool, String(chatId), 1000)).toEqual([
      { role: "user", content: "раз" },
      { role: "assistant", content: "два" },
      { role: "user", content: "три" },
    ]);
  });

  it("чат-изоляция по обеим сторонам: чужие входящие и чужие ответы не видны", async () => {
    await insertIncoming(806, "моё входящее", 10);
    await insertSentReply(806, "мой ответ", 9);
    await insertIncoming(807, "чужое входящее", 8);
    await insertSentReply(807, "чужой ответ", 7);

    expect(await recentHistory(pool, "806", 100)).toEqual([
      { role: "user", content: "моё входящее" },
      { role: "assistant", content: "мой ответ" },
    ]);
    expect(await recentHistory(pool, "807", 100)).toEqual([
      { role: "user", content: "чужое входящее" },
      { role: "assistant", content: "чужой ответ" },
    ]);
  });
});

afterAll(async () => {
  await pool.end();
});
