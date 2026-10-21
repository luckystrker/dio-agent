// Интеграционный тест (живой PostgreSQL, база dio_test).
// §6.2: буферы чатов — события попадают в turn транзакционно (turn,
// привязка событий, work_item — атомарно), статус buffered; окна
// idle/max по классам; закрытие по 20-м событию и лимиту текста;
// recoverStalePending подбирает потерянные pending (§12.2 строка 1).
// Фейковые таймеры фейкуют только setTimeout/clearTimeout: I/O к БД
// остаётся настоящим и ждётся через setImmediate-поллинг (waitForDb).

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Pool } from "pg";
import { ChatBufferManager, MAX_BATCH_TEXT_CHARS, MAX_EVENTS_PER_BATCH, windowsFor } from "./buffers.js";
import { createTestPool } from "../test-support/db.js";
import {
  cleanTelegramTables,
  insertEvent,
  silentLogger,
  waitForDb,
} from "../test-support/telegram-db.js";

let pool: Pool;
let nextUpdateId = 1_000;

beforeEach(async () => {
  pool = createTestPool();
  await cleanTelegramTables(pool);
  nextUpdateId = 1_000;
  // Только таймеры: Date и setImmediate настоящие — waitForDb продолжает работать
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
});

afterEach(async () => {
  vi.useRealTimers();
  await pool.end();
});

async function addPending(
  manager: ChatBufferManager,
  options: {
    chatId: string;
    chatType: "private" | "group";
    kind?: "message" | "reply" | "mention" | "command" | "edited" | "other";
    text: string;
  },
): Promise<string> {
  const eventId = await insertEvent(pool, {
    telegramUpdateId: nextUpdateId++,
    chatId: options.chatId,
    chatType: options.chatType,
    kind: options.kind ?? "message",
    text: options.text,
    status: "pending",
  });
  manager.add({
    eventId,
    chatId: options.chatId,
    chatType: options.chatType,
    kind: options.kind ?? "message",
    text: options.text,
    occurredAt: new Date(),
  });
  return eventId;
}

async function turnsCount(): Promise<number> {
  const result = await pool.query<{ n: string }>("SELECT count(*)::text AS n FROM turns");
  return Number(result.rows[0]!.n);
}

async function eventsOfTurn(turnId: string): Promise<string[]> {
  const result = await pool.query<{ id: string; status: string }>(
    "SELECT id::text, status FROM social_events WHERE turn_id = $1 ORDER BY id ASC",
    [turnId],
  );
  for (const row of result.rows) expect(row.status).toBe("buffered");
  return result.rows.map((row) => row.id);
}

async function turnOfWorkItem(payloadTurnId: string) {
  const work = await pool.query<{ id: string; kind: string; payload: unknown }>(
    "SELECT id::text, kind, payload FROM work_items WHERE payload->>'turnId' = $1",
    [payloadTurnId],
  );
  return work.rows[0];
}

describe("ChatBufferManager: закрытие порции в turn (§6.2)", () => {
  it("idle-закрытие: события попадают в turn, work_item создаётся, статус buffered", async () => {
    const manager = new ChatBufferManager({ pool, logger: silentLogger });
    const first = await addPending(manager, { chatId: "500", chatType: "private", text: "первое" });
    const second = await addPending(manager, { chatId: "500", chatType: "private", text: "второе" });

    await vi.advanceTimersByTimeAsync(1_499);
    expect(await turnsCount()).toBe(0); // idle-окно лички 1–2 с ещё не истекло

    await vi.advanceTimersByTimeAsync(2);
    await waitForDb(async () => (await turnsCount()) === 1, "idle-закрытие в turn");

    const turn = await pool.query<{ id: string; chat_id: string; status: string }>(
      "SELECT id::text, chat_id::text, status FROM turns",
    );
    const turnId = turn.rows[0]!.id;
    expect(turn.rows[0]!.chat_id).toBe("500");

    const ids = await eventsOfTurn(turnId);
    expect(ids).toEqual([first, second]);

    const work = await turnOfWorkItem(turnId);
    expect(work).toMatchObject({ kind: "direct" });
    expect(work?.payload).toMatchObject({ turnId, chatId: "500" });
  });

  it("max-закрытие от первого события: непрерывный поток не переносит закрытие бесконечно", async () => {
    const manager = new ChatBufferManager({ pool, logger: silentLogger });
    // события каждые 1400 мс — чаще idle лички (1500), но max (3000) от первого события
    await addPending(manager, { chatId: "501", chatType: "private", text: "t0" });
    await vi.advanceTimersByTimeAsync(1_400);
    await addPending(manager, { chatId: "501", chatType: "private", text: "t1400" });
    await vi.advanceTimersByTimeAsync(1_400);
    await addPending(manager, { chatId: "501", chatType: "private", text: "t2800" });
    expect(await turnsCount()).toBe(0); // ещё не 3000 от первого события

    await vi.advanceTimersByTimeAsync(250); // t=3050 > max 3000
    await waitForDb(async () => (await turnsCount()) === 1, "max-закрытие");

    const turn = await pool.query<{ id: string }>("SELECT id::text FROM turns");
    expect(await eventsOfTurn(turn.rows[0]!.id)).toHaveLength(3);
  });

  it("окно fast-класса: reply в группе закрывается в пределах 0–300 мс (§6.2)", async () => {
    const manager = new ChatBufferManager({ pool, logger: silentLogger });
    expect(windowsFor({ chatType: "group", kind: "reply" }).idleMs).toBeLessThanOrEqual(300);

    await addPending(manager, { chatId: "-100", chatType: "group", kind: "reply", text: "reply" });
    await vi.advanceTimersByTimeAsync(100);
    expect(await turnsCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(150); // суммарно 250 мс — внутри fast-окна
    await waitForDb(async () => (await turnsCount()) === 1, "fast-закрытие reply");

    const work = await pool.query<{ kind: string }>("SELECT kind FROM work_items");
    expect(work.rows[0]?.kind).toBe("group");
  });

  it("окно пассивной группы: не закрывается раньше 3 с, закрывается к 5 с", async () => {
    const manager = new ChatBufferManager({ pool, logger: silentLogger });
    await addPending(manager, { chatId: "-200", chatType: "group", kind: "message", text: "шум" });
    await vi.advanceTimersByTimeAsync(2_900);
    expect(await turnsCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(1_200); // t=4100 — внутри 3–5 с
    await waitForDb(async () => (await turnsCount()) === 1, "закрытие пассивной группы");
  });

  it("закрытие на 20-м событии: 19 не закрывают, 20-е закрывает немедленно", async () => {
    const manager = new ChatBufferManager({ pool, logger: silentLogger });
    for (let i = 0; i < MAX_EVENTS_PER_BATCH - 1; i++) {
      await addPending(manager, { chatId: "502", chatType: "private", text: `сообщение ${i}` });
    }
    expect(await turnsCount()).toBe(0); // 19 < 20 — порция ещё собирается

    await addPending(manager, { chatId: "502", chatType: "private", text: "двадцатое" });
    await waitForDb(async () => (await turnsCount()) === 1, "закрытие по 20-му событию");

    const turn = await pool.query<{ id: string }>("SELECT id::text FROM turns");
    expect(await eventsOfTurn(turn.rows[0]!.id)).toHaveLength(MAX_EVENTS_PER_BATCH);
  });

  it("закрытие по лимиту входного текста: порог MAX_BATCH_TEXT_CHARS", async () => {
    const manager = new ChatBufferManager({ pool, logger: silentLogger });
    await addPending(manager, {
      chatId: "503",
      chatType: "private",
      text: "а".repeat(MAX_BATCH_TEXT_CHARS - 1),
    });
    expect(await turnsCount()).toBe(0); // на единицу ниже лимита

    await addPending(manager, { chatId: "503", chatType: "private", text: "ещё чуть-чуть" });
    await waitForDb(async () => (await turnsCount()) === 1, "закрытие по лимиту текста");

    const turn = await pool.query<{ id: string }>("SELECT id::text FROM turns");
    expect(await eventsOfTurn(turn.rows[0]!.id)).toHaveLength(2);
  });

  it("точная граница лимита: одно событие ровно MAX_BATCH_TEXT_CHARS закрывает порцию немедленно", async () => {
    // граница включительно: мутация >= на > оставляла бы ровно-лимитное
    // событие в буфере до таймера — тест бы повис на waitForDb
    const manager = new ChatBufferManager({ pool, logger: silentLogger });
    await addPending(manager, {
      chatId: "505",
      chatType: "private",
      text: "б".repeat(MAX_BATCH_TEXT_CHARS),
    });
    await waitForDb(async () => (await turnsCount()) === 1, "закрытие ровно на границе лимита");

    const turn = await pool.query<{ id: string }>("SELECT id::text FROM turns");
    expect(await eventsOfTurn(turn.rows[0]!.id)).toHaveLength(1);
  });

  it("буферы разных чатов независимы: закрытие одного не трогает другой", async () => {
    const manager = new ChatBufferManager({ pool, logger: silentLogger });
    await addPending(manager, { chatId: "600", chatType: "private", text: "в личке" });
    await addPending(manager, { chatId: "-300", chatType: "group", kind: "reply", text: "в группе" });

    await vi.advanceTimersByTimeAsync(1_600); // личка закрылась, группа (reply, 150 мс) — давно
    await waitForDb(async () => (await turnsCount()) === 2, "два независимых буфера");

    const turns = await pool.query<{ chat_id: string }>(
      "SELECT chat_id::text FROM turns ORDER BY chat_id",
    );
    expect(turns.rows.map((row) => row.chat_id)).toEqual(["-300", "600"]);
  });

  it("flushAll закрывает все буферы немедленно (graceful shutdown)", async () => {
    const manager = new ChatBufferManager({ pool, logger: silentLogger });
    await addPending(manager, { chatId: "700", chatType: "private", text: "раз" });
    await addPending(manager, { chatId: "701", chatType: "private", text: "два" });

    await manager.flushAll();
    expect(await turnsCount()).toBe(2); // без ожидания таймеров

    const statuses = await pool.query<{ status: string }>(
      "SELECT status FROM social_events ORDER BY id",
    );
    expect(statuses.rows.map((row) => row.status)).toEqual(["buffered", "buffered"]);
  });
});

describe("ChatBufferManager: транзакционность и recovery (§6.2, §12.2)", () => {
  it("сбой постановки работы откатывает всё: нет turn, события остаются pending (и подбираются recovery)", async () => {
    const manager = new ChatBufferManager({
      pool,
      logger: silentLogger,
      enqueueWork: async () => {
        throw new Error("очередь недоступна");
      },
    });
    const eventId = await addPending(manager, { chatId: "800", chatType: "private", text: "потеряется?" });

    await vi.advanceTimersByTimeAsync(1_600);
    // даём асинхронному close() и его ROLLBACK доехать (реальное время;
    // фейковые таймеры не мешают I/O)
    const deadline = Date.now() + 1_000;
    while (Date.now() < deadline) {
      await new Promise((resolve) => setImmediate(resolve));
    }

    expect(await turnsCount()).toBe(0); // turn откатился
    const event = await pool.query<{ status: string; turn_id: string | null }>(
      "SELECT status, turn_id::text FROM social_events WHERE id = $1",
      [eventId],
    );
    expect(event.rows[0]).toMatchObject({ status: "pending", turn_id: null });

    // события не потеряны: recovery после «перезапуска» закрывает их в turn
    const revived = new ChatBufferManager({ pool, logger: silentLogger });
    const recovered = await revived.recoverStalePending();
    expect(recovered).toEqual({ chats: 1, events: 1 });
    expect(await eventsOfTurn((await pool.query<{ id: string }>("SELECT id::text FROM turns")).rows[0]!.id)).toEqual([eventId]);
  });

  it("recoverStalePending: pending-события без turn закрываются по чатам; чужие статусы не трогаются", async () => {
    const lost1 = await insertEvent(pool, {
      telegramUpdateId: 1,
      chatId: "900",
      chatType: "private",
      text: "потерянное 1",
    });
    const lost2 = await insertEvent(pool, {
      telegramUpdateId: 2,
      chatId: "900",
      chatType: "private",
      text: "потерянное 2",
    });
    const lostGroup = await insertEvent(pool, {
      telegramUpdateId: 3,
      chatId: "-400",
      chatType: "group",
      kind: "reply",
      text: "потерянное в группе",
    });
    const journalOnly = await insertEvent(pool, {
      telegramUpdateId: 4,
      chatId: "900",
      chatType: "group",
      kind: "other",
      text: null,
      status: "journal_only",
    });
    const alreadyTurn = await pool.query<{ id: string }>(
      "INSERT INTO turns (chat_id) VALUES ('901') RETURNING id::text",
    );
    const buffered = await insertEvent(pool, {
      telegramUpdateId: 5,
      chatId: "901",
      chatType: "private",
      text: "уже в turn",
      status: "buffered",
    });
    await pool.query("UPDATE social_events SET turn_id = $2, status = 'buffered' WHERE id = $1", [
      buffered,
      alreadyTurn.rows[0]!.id,
    ]);

    const manager = new ChatBufferManager({ pool, logger: silentLogger });
    const recovered = await manager.recoverStalePending();

    expect(recovered).toEqual({ chats: 2, events: 3 }); // 900: два, -400: одно
    expect(await turnsCount()).toBe(3); // новый на 900, новый на -400, старый на 901

    const privateTurn = await pool.query<{ id: string }>(
      "SELECT t.id::text FROM turns t JOIN social_events e ON e.turn_id = t.id WHERE e.id = $1",
      [lost1],
    );
    expect(await eventsOfTurn(privateTurn.rows[0]!.id)).toEqual([lost1, lost2]);

    const groupWork = await pool.query<{ kind: string }>(
      "SELECT kind FROM work_items WHERE payload->>'turnId' = (SELECT turn_id::text FROM social_events WHERE id = $1)",
      [lostGroup],
    );
    expect(groupWork.rows[0]?.kind).toBe("group");

    // journal_only и уже закрытые события не тронуты
    const untouched = await pool.query<{ status: string; turn_id: string | null }>(
      "SELECT status, turn_id::text FROM social_events WHERE id = ANY($1)",
      [[journalOnly, buffered]],
    );
    expect(untouched.rows.find((row) => row.status === "journal_only")).toBeTruthy();
    expect(
      untouched.rows.find((row) => row.turn_id === alreadyTurn.rows[0]!.id),
    ).toBeTruthy();
  });
});
