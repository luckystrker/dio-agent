// Интеграционный тест (живой PostgreSQL, база dio_test): запись памяти
// после хода и Г-критерий «модель не может сослаться на несуществующий
// источник» (§10.1, §8.3, §11.3). Путь целиком: модель → strict-схема →
// валидация кандидатов → транзакция применения → memory_items + links +
// sources. Источник turn:N назначает runtime; посторонние поля у кандидата
// отклоняются всей схемой (нет «лучшего усилия»), в память не попадает ничего.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { makeTurnHandler } from "./turn-handler.js";
import { DEFAULT_PERSONA } from "./persona.js";
import { MoodRepo } from "./mood.js";
import { RelationshipRepo } from "./relationships.js";
import type { CharacterModel } from "./model-adapter.js";
import { OpenAICompatModelAdapter, type ModelTransport } from "./model-adapter.js";
import type { TurnResult } from "./turn-result.js";
import { ModelError } from "./model-adapter.js";
import { MemoryRepo } from "../memory/memory-repo.js";
import { CandidateError } from "../memory/memory-candidates.js";
import { chatScope } from "../memory/scope.js";
import { OutboxRepo } from "../telegram/outbox.js";
import { IntentionRepo } from "../background/intentions.js";
import { WorkQueueRepo, type WorkItem } from "../runtime/work-queue.js";
import { cleanMemoryTables, createTestPool } from "../test-support/db.js";
import { cleanTelegramTables, silentLogger } from "../test-support/telegram-db.js";

let pool: Pool;
let outbox: OutboxRepo;
let workRepo: WorkQueueRepo;
let memory: MemoryRepo;
let nextUpdateId = 7_000;

beforeEach(async () => {
  pool = createTestPool();
  outbox = new OutboxRepo(pool);
  workRepo = new WorkQueueRepo(pool);
  memory = new MemoryRepo(pool);
  await cleanTelegramTables(pool);
  await cleanMemoryTables(pool);
  nextUpdateId = 7_000;
});

afterEach(async () => {
  // порядок db-файлов не гарантирован: таблицы памяти чистим и после прогона,
  // чтобы чужие строгие ассерты (напр. handle-turn) не видели наши записи
  await cleanMemoryTables(pool);
  await pool.end();
});

interface CapturedCall {
  system: string;
  userTexts: string[];
}

/** Адаптер с заглушкой транспорта: отвечает заданным tool-аргументами. */
function makeModel(
  toolArguments: unknown,
  options: { maxRetries?: number } = {},
): { adapter: OpenAICompatModelAdapter; calls: CapturedCall[] } {
  const calls: CapturedCall[] = [];
  const transport: ModelTransport = async (_url, init) => {
    const body = JSON.parse(init.body) as { messages: Array<{ role: string; content: string }> };
    calls.push({
      system: body.messages[0]?.content ?? "",
      userTexts: body.messages.filter((m) => m.role === "user").map((m) => m.content),
    });
    return new Response(
      JSON.stringify({
        choices: [
          {
            message: {
              tool_calls: [
                {
                  type: "function",
                  function: { name: "turn_result", arguments: JSON.stringify(toolArguments) },
                },
              ],
            },
          },
        ],
        usage: { prompt_tokens: 10, completion_tokens: 5 },
      }),
      { status: 200 },
    );
  };
  return {
    adapter: new OpenAICompatModelAdapter({
      endpoint: "https://model.example/v1",
      apiKey: "key",
      model: "test-model",
      transport,
      maxRetries: options.maxRetries ?? 1,
    }),
    calls,
  };
}

/** Модель-заглушка в обход адаптера: возвращает произвольную память как есть. */
function makeRawModel(memoryOverride: unknown): CharacterModel {
  return {
    async runTurn(): Promise<TurnResult> {
      return {
        decision: "answer",
        messages: ["ответ"],
        notes: [],
        memory: memoryOverride,
      } as unknown as TurnResult;
    },
  };
}

/** Прямое создание turn + события + work item, как сделали бы буферы этапа 2. */
async function makeTurn(chatId: string, texts: string[]): Promise<{ turnId: string; item: WorkItem }> {
  const turn = await pool.query<{ id: string }>(
    "INSERT INTO turns (chat_id) VALUES ($1) RETURNING id::text",
    [chatId],
  );
  const turnId = turn.rows[0]!.id;
  for (const text of texts) {
    await pool.query(
      `INSERT INTO social_events
         (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status, turn_id)
       VALUES ($1, $2, 'private', '42', 'message', $3, '{}', 'pending', $4)`,
      [nextUpdateId++, chatId, text, turnId],
    );
  }
  const itemId = await workRepo.enqueue("direct", { turnId, chatId });
  const item = await workRepo.claimNext();
  expect(item!.id).toBe(itemId);
  return { turnId, item: item! };
}

const PERSONA_STUB = {
  id: "0",
  version: 1,
  name: DEFAULT_PERSONA.name,
  description: DEFAULT_PERSONA.description,
  speechStyle: DEFAULT_PERSONA.speechStyle,
  values: [...DEFAULT_PERSONA.values],
  moodBaseline: { ...DEFAULT_PERSONA.moodBaseline },
  seedBio: DEFAULT_PERSONA.seedBio,
};

function handlerWith(model: CharacterModel | null) {
  return makeTurnHandler({
    pool,
    model,
    outbox,
    memory,
    intentions: new IntentionRepo(pool),
    persona: PERSONA_STUB,
    mood: new MoodRepo(pool),
    relationships: new RelationshipRepo(pool),
    workQueue: new WorkQueueRepo(pool),
    modelBudget: null,
    modelDailyCallsLimit: 100,
    logger: silentLogger,
  });
}

async function memoryRows() {
  const items = await pool.query<{
    id: string;
    kind: string;
    text: string;
    scope_id: string;
    status: string;
    salience: string;
    confidence: string | null;
  }>(
    "SELECT id::text, kind, text, scope_id, status, salience::text, confidence::text FROM memory_items ORDER BY id",
  );
  const sources = await pool.query<{ kind: string; ref: string; memory_ids: string[] }>(
    `SELECT s.kind, s.ref, array_agg(ms.memory_id::text ORDER BY ms.memory_id) AS memory_ids
     FROM sources s LEFT JOIN memory_sources ms ON ms.source_id = s.id
     GROUP BY s.id, s.kind, s.ref ORDER BY s.ref`,
  );
  const entities = await pool.query<{ id: string; name: string; scope_id: string }>(
    "SELECT id::text, canonical_name AS name, scope_id FROM entities ORDER BY id",
  );
  const links = await pool.query<{ memory_id: string; entity_id: string; role: string }>(
    "SELECT memory_id::text, entity_id::text, role FROM memory_links ORDER BY memory_id, entity_id",
  );
  return { items: items.rows, sources: sources.rows, entities: entities.rows, links: links.rows };
}

const SIGNAL = () => new AbortController().signal;

describe("запись после хода (§10.1): валидные кандидаты пишутся транзакционно", () => {
  it("memory_items + links + sources: источник turn:N назначен runtime, scope = chat-скоп хода", async () => {
    const { turnId, item } = await makeTurn("500", ["я коллекционирую минералы"]);
    const { adapter } = makeModel({
      decision: "answer",
      messages: ["отличное увлечение"],
      notes: [],
      memory: [
        {
          kind: "claim",
          text: "Пользователь коллекционирует минералы",
          entities: [{ name: "минералы", kind: "topic", role: "subject" }],
          salience: 0.9,
          confidence: 0.8,
        },
        {
          kind: "experience",
          text: "Обсуждали коллекцию минералов",
          entities: [{ name: "минералы", kind: "topic", role: "context" }],
        },
      ],
    });

    await handlerWith(adapter)(item, SIGNAL());

    const state = await memoryRows();
    expect(state.items).toHaveLength(2);
    const [claim, experience] = state.items;
    expect(claim).toMatchObject({
      kind: "claim",
      text: "Пользователь коллекционирует минералы",
      scope_id: chatScope("500"),
      status: "active",
      salience: "0.9",
      confidence: "0.8",
    });
    expect(experience).toMatchObject({
      kind: "experience",
      text: "Обсуждали коллекцию минералов",
      scope_id: chatScope("500"),
      status: "active",
      salience: "0.5", // дефолт назначен runtime
      confidence: null,
    });

    // один источник на ход: journal_event → turn:N, связан с обеими записями
    expect(state.sources).toEqual([
      { kind: "journal_event", ref: `turn:${turnId}`, memory_ids: [claim!.id, experience!.id] },
    ]);

    // сущности в scope хода: карточка собеседника (runtime, §7.3)
    // + «минералы», переиспользованная обоими кандидатами
    expect(state.entities).toEqual([
      { id: expect.any(String), name: "user:42", scope_id: chatScope("500") },
      { id: expect.any(String), name: "минералы", scope_id: chatScope("500") },
    ]);
    const minerals = state.entities.find((row) => row.name === "минералы")!;
    expect(state.links).toEqual([
      { memory_id: claim!.id, entity_id: minerals.id, role: "subject" },
      { memory_id: experience!.id, entity_id: minerals.id, role: "context" },
    ]);

    // ход применён, события обработаны
    const turn = await pool.query<{ status: string }>("SELECT status FROM turns WHERE id = $1", [turnId]);
    expect(turn.rows[0]!.status).toBe("applied");
    const events = await pool.query<{ status: string }>(
      "SELECT status FROM social_events WHERE turn_id = $1",
      [turnId],
    );
    expect(events.rows.every((row) => row.status === "processed")).toBe(true);
  });
});

describe("Г-критерий: модель не может сослаться на несуществующий источник", () => {
  it("шесть кандидатов (лимит 5, §10.1): весь ответ невалиден — retry, ошибка, в памяти ничего", async () => {
    const { turnId, item } = await makeTurn("500", ["сообщение"]);
    const six = Array.from({ length: 6 }, (_unused, i) => ({
      kind: "experience",
      text: `значимый факт ${i}`,
    }));
    const { adapter, calls } = makeModel({
      decision: "answer",
      messages: ["ответ"],
      notes: [],
      memory: six,
    });

    await expect(handlerWith(adapter)(item, SIGNAL())).rejects.toBeInstanceOf(ModelError);
    // strict-схема отклонила ответ; адаптер сделал ровно retry и сдался (§11.3)
    expect(calls).toHaveLength(2);

    const state = await memoryRows();
    expect(state.items).toEqual([]);
    expect(state.sources).toEqual([]);
    const turn = await pool.query<{ status: string }>("SELECT status FROM turns WHERE id = $1", [turnId]);
    expect(turn.rows[0]!.status).toBe("created");
    const events = await pool.query<{ status: string }>(
      "SELECT status FROM social_events WHERE turn_id = $1",
      [turnId],
    );
    expect(events.rows.every((row) => row.status === "pending")).toBe(true);
    const outboxRows = await pool.query("SELECT 1 FROM delivery_outbox WHERE turn_id = $1", [turnId]);
    expect(outboxRows.rows).toEqual([]);
  });

  it("посторонние поля у кандидата (source/id/created_by) — strict-схема отклоняет весь ответ", async () => {
    const { turnId, item } = await makeTurn("500", ["сообщение"]);
    const { adapter, calls } = makeModel({
      decision: "answer",
      messages: ["ответ"],
      notes: [],
      memory: [
        {
          kind: "claim",
          text: "в целом валидный кандидат",
          source: "web_document:url:https://example.com/fake",
          id: 999,
          created_by: "model",
        },
      ],
    });

    await expect(handlerWith(adapter)(item, SIGNAL())).rejects.toBeInstanceOf(ModelError);
    expect(calls).toHaveLength(2);

    // в память не попало НИЧЕГО — в том числе не создан «подделанный» источник
    const state = await memoryRows();
    expect(state.items).toEqual([]);
    expect(state.sources).toEqual([]);
    const turn = await pool.query<{ status: string }>("SELECT status FROM turns WHERE id = $1", [turnId]);
    expect(turn.rows[0]!.status).toBe("created");
  });

  it("валидатор обработчика (в обход адаптера): битый кандидат валит весь набор, не «лучшее усилие»", async () => {
    const { turnId, item } = await makeTurn("500", ["сообщение"]);
    const model = makeRawModel({
      memory: [
        { kind: "claim", text: "валидный кандидат" },
        { kind: "vibe", text: "невалидный вид" },
      ],
    });

    await expect(handlerWith(model)(item, SIGNAL())).rejects.toBeInstanceOf(CandidateError);

    const state = await memoryRows();
    expect(state.items).toEqual([]); // валидный тоже не записан — набор отклонён целиком
    expect(state.sources).toEqual([]);
    const turn = await pool.query<{ status: string }>("SELECT status FROM turns WHERE id = $1", [turnId]);
    expect(turn.rows[0]!.status).toBe("created");
  });
});

describe("записи доступны следующему ходу сразу и изолированы по scope (§10.1, §9.2)", () => {
  const MEMORIZED = "Пользователь коллекционирует минералы";

  it("следующий ход того же чата видит запись в контексте; чужой чат — нет", async () => {
    // ход 1 (чат 500): модель оставляет значимую запись
    const first = await makeTurn("500", ["я коллекционирую минералы"]);
    const writer = makeModel({
      decision: "answer",
      messages: ["запомню"],
      notes: [],
      memory: [{ kind: "claim", text: MEMORIZED }],
    });
    await handlerWith(writer.adapter)(first.item, SIGNAL());

    // ход 2 (тот же чат): контекст содержит запись памяти
    const second = await makeTurn("500", ["расскажи про минералы"]);
    const reader = makeModel({ decision: "answer", messages: ["ответ"], notes: [] });
    await handlerWith(reader.adapter)(second.item, SIGNAL());
    expect(reader.calls).toHaveLength(1);
    expect(reader.calls[0]!.system).toContain(MEMORIZED);

    // ход 3 (чужой чат 501): карточка памяти чата 500 не попадает в контекст
    const third = await makeTurn("501", ["а у него были минералы?"]);
    const stranger = makeModel({ decision: "answer", messages: ["ответ"], notes: [] });
    await handlerWith(stranger.adapter)(third.item, SIGNAL());
    expect(stranger.calls).toHaveLength(1);
    // система — persona-промпт (с mood/relationship-строками), без памяти чужого чата
    expect(stranger.calls[0]!.system).toContain(PERSONA_STUB.name);
    expect(stranger.calls[0]!.system.includes(MEMORIZED)).toBe(false);
  });
});
