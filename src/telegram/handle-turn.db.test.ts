// Интеграционный тест (живой PostgreSQL, база dio_test).
// Настоящий makeTurnHandler (src/character/turn-handler.ts): события →
// контекст → модель → CAS-транзакция применения. Инвариант 4 §12.1 —
// применение хода однократно: (a) применение создаёт outbox + processed +
// дельты; (b) повторный вызов с тем же turnId — ноль дублей; (c) конкурентный
// двойной вызов — побеждает один. Итерация 2: SQL-реплика index.ts удалена,
// тестируется сам обработчик — мутация сломанного CAS валит тест (b).

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { ChatBufferManager } from "./buffers.js";
import { OutboxRepo } from "./outbox.js";
import { makeTurnHandler } from "../character/turn-handler.js";
import { MemoryRepo } from "../memory/memory-repo.js";
import { MoodRepo } from "../character/mood.js";
import { RelationshipRepo } from "../character/relationships.js";
import { DEFAULT_PERSONA } from "../character/persona.js";
import { OpenAICompatModelAdapter, type ModelTransport } from "../character/model-adapter.js";
import { SkipWorkError, type WorkHandler } from "../runtime/worker.js";
import { WorkQueueRepo, type WorkItem } from "../runtime/work-queue.js";
import { createTestPool } from "../test-support/db.js";
import { cleanTelegramTables, silentLogger } from "../test-support/telegram-db.js";

let pool: Pool;
let outbox: OutboxRepo;
let workRepo: WorkQueueRepo;
let nextUpdateId = 5_000;

beforeEach(async () => {
  pool = createTestPool();
  outbox = new OutboxRepo(pool);
  workRepo = new WorkQueueRepo(pool);
  await cleanTelegramTables(pool);
  nextUpdateId = 5_000;
});

afterEach(async () => {
  await pool.end();
});

/** Транспорт-заглушка: перехватывает тело запроса, отвечает валидным tool-вызовом. */
function makeStubModel(calls: Array<{ messages: Array<{ role: string; content: string }> }>): OpenAICompatModelAdapter {
  const transport: ModelTransport = async (_url, init) => {
    const body = JSON.parse(init.body) as { messages: Array<{ role: string; content: string }> };
    calls.push({ messages: body.messages });
    return new Response(
      JSON.stringify({
        choices: [
          {
            message: {
              tool_calls: [
                {
                  type: "function",
                  function: {
                    name: "turn_result",
                    arguments: JSON.stringify({
                      decision: "answer",
                      messages: ["ответ первый", "ответ второй"],
                      notes: ["пользователь написал два сообщения", "тон спокойный"],
                    }),
                  },
                },
              ],
            },
          },
        ],
        usage: { prompt_tokens: 11, completion_tokens: 7 },
      }),
      { status: 200 },
    );
  };
  return new OpenAICompatModelAdapter({
    endpoint: "https://model.example/v1",
    apiKey: "key",
    model: "test-model",
    transport,
  });
}

const DEFAULT_PERSONA_STUB = {
  id: "0",
  version: 1,
  name: DEFAULT_PERSONA.name,
  description: DEFAULT_PERSONA.description,
  speechStyle: DEFAULT_PERSONA.speechStyle,
  values: [...DEFAULT_PERSONA.values],
  moodBaseline: { ...DEFAULT_PERSONA.moodBaseline },
  seedBio: DEFAULT_PERSONA.seedBio,
};

function makeHandler(model: OpenAICompatModelAdapter | null): WorkHandler {
  return makeTurnHandler({
    pool,
    model,
    outbox,
    memory: new MemoryRepo(pool),
    persona: DEFAULT_PERSONA_STUB,
    mood: new MoodRepo(pool),
    relationships: new RelationshipRepo(pool),
    workQueue: new WorkQueueRepo(pool),
    modelBudget: null,
    modelDailyCallsLimit: 100,
    logger: silentLogger,
  });
}

/** Реальный путь буферов: события → flushAll → turn + work_item. */
async function makeTurnWithEvents(
  texts: string[],
  chatId = "500",
): Promise<{ turnId: string; item: WorkItem }> {
  const manager = new ChatBufferManager({ pool, logger: silentLogger });
  for (const text of texts) {
    const inserted = await pool.query<{ id: string }>(
      `INSERT INTO social_events
         (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status)
       VALUES ($1, $2, 'private', '42', 'message', $3, $4, 'pending')
       RETURNING id::text`,
      [nextUpdateId++, chatId, text, JSON.stringify({ stub: true })],
    );
    manager.add({
      eventId: inserted.rows[0]!.id,
      chatId,
      chatType: "private",
      kind: "message",
      text,
      occurredAt: new Date(),
    });
  }
  await manager.flushAll();

  const turn = await pool.query<{ id: string }>(
    "SELECT id::text FROM turns WHERE chat_id = $1 ORDER BY id DESC LIMIT 1",
    [chatId],
  );
  const turnId = turn.rows[0]!.id;
  const item = await workRepo.claimNext();
  expect(item).not.toBeNull();
  expect(item!.payload).toMatchObject({ turnId, chatId });
  return { turnId, item: item! };
}

async function stateOf(turnId: string) {
  const turn = await pool.query<{ status: string }>("SELECT status FROM turns WHERE id = $1", [turnId]);
  const outboxRows = await pool.query<{ position: number; status: string; text: string }>(
    "SELECT position, status, payload->>'text' AS text FROM delivery_outbox WHERE turn_id = $1 ORDER BY position",
    [turnId],
  );
  const notes = await pool.query<{ work_item_id: string; text: string }>(
    "SELECT work_item_id::text, text FROM state_notes ORDER BY id",
  );
  const events = await pool.query<{ status: string }>(
    "SELECT status FROM social_events WHERE turn_id = $1 ORDER BY id",
    [turnId],
  );
  return { turn: turn.rows[0]!, outboxRows: outboxRows.rows, notes: notes.rows, events: events.rows };
}

describe("makeTurnHandler: применение хода (§12.1, инвариант 4)", () => {
  it("(a) первое применение: turn applied, события processed, outbox-строки и дельты записаны; контекст собран из событий", async () => {
    const { turnId, item } = await makeTurnWithEvents(["первое сообщение", "второе сообщение"]);
    const modelCalls: Array<{ messages: Array<{ role: string; content: string }> }> = [];
    const handler = makeHandler(makeStubModel(modelCalls));

    await handler(item, new AbortController().signal);

    // контекст собран приложением явно: persona-system (с mood/relationship)
    // + события как user-сообщения
    expect(modelCalls).toHaveLength(1);
    expect(modelCalls[0]!.messages).toEqual([
      { role: "system", content: expect.stringContaining(DEFAULT_PERSONA_STUB.name) },
      { role: "user", content: "первое сообщение" },
      { role: "user", content: "второе сообщение" },
    ]);

    const state = await stateOf(turnId);
    expect(state.turn.status).toBe("applied");
    expect(state.events.map((row) => row.status)).toEqual(["processed", "processed"]);
    expect(state.outboxRows.map((row) => [row.position, row.status, row.text])).toEqual([
      [0, "pending", "ответ первый"],
      [1, "pending", "ответ второй"],
    ]);
    expect(state.notes.map((row) => row.text)).toEqual([
      "пользователь написал два сообщения",
      "тон спокойный",
    ]);
    expect(state.notes.every((row) => row.work_item_id === item.id)).toBe(true);
  });

  it("(b) повторный вызов с тем же turnId: CAS отклоняет, outbox и дельты не дублируются", async () => {
    const { turnId, item } = await makeTurnWithEvents(["сообщение"]);
    const modelCalls: Array<{ messages: Array<{ role: string; content: string }> }> = [];
    const handler = makeHandler(makeStubModel(modelCalls));

    await handler(item, new AbortController().signal);
    const afterFirst = await stateOf(turnId);

    // «сбой после commit»: тот же turn повторно (requeue после падения процесса)
    await handler(item, new AbortController().signal);
    const afterSecond = await stateOf(turnId);

    expect(afterSecond.turn.status).toBe("applied");
    expect(afterSecond.outboxRows).toHaveLength(afterFirst.outboxRows.length); // 2, не 4
    expect(afterSecond.outboxRows).toEqual(afterFirst.outboxRows);
    expect(afterSecond.notes).toHaveLength(afterFirst.notes.length); // 2, не 4
    expect(afterSecond.notes).toEqual(afterFirst.notes);
    // 2.7: применённый turn закрывается БЕЗ повторного вызова модели —
    // «ответ хода переживает процесс», повторного применения нет
    expect(modelCalls).toHaveLength(1);
  });

  it("(c) конкурентный двойной вызов одного turn: применяется ровно один", async () => {
    const { turnId, item: first } = await makeTurnWithEvents(["сообщение"]);
    // второй work-запрос на тот же turn (двойной захват после сбоя)
    const duplicateId = await workRepo.enqueue("direct", { turnId, chatId: "500" });
    const duplicate = await workRepo.claimNext();
    expect(duplicate!.id).toBe(duplicateId);

    const handler = makeHandler(makeStubModel([]));
    await Promise.all([
      handler(first, new AbortController().signal),
      handler(duplicate!, new AbortController().signal),
    ]);

    const state = await stateOf(turnId);
    expect(state.outboxRows).toHaveLength(2); // ровно один набор пузырей
    expect(state.notes).toHaveLength(2); // ровно один набор дельт
    // все заметки принадлежат одному (победившему) work_item
    const owners = new Set(state.notes.map((row) => row.work_item_id));
    expect(owners.size).toBe(1);
    expect([first.id, duplicateId]).toContain([...owners][0]);
    expect(state.events.every((row) => row.status === "processed")).toBe(true);
  });

  it("модель не настроена — осознанный пропуск SkipWorkError, а не тихое «ничего не сделал»", async () => {
    const handler = makeHandler(null);
    const id = await workRepo.enqueue("direct", { turnId: "1", chatId: "500" });
    const item = await workRepo.claimNext();
    expect(item!.id).toBe(id);

    await expect(handler(item!, new AbortController().signal)).rejects.toBeInstanceOf(SkipWorkError);
  });
});
