// Интеграционный тест (живой PostgreSQL, база dio_test).
// Этап 4 «Персонаж» — полный путь makeTurnHandler со стаб-моделью (§7, §12.1–12.2,
// §11.2, §13, чек-лист 1.9/1.10/2.7):
// - mood_delta применяется и пишет заметку-основание; relationship_delta
//   применяется + familiarity растёт; person-entity создаётся из actor_name;
// - result_ready: сбой применения ПОСЛЕ сохранения результата — повторный вызов
//   применяет сохранённый, модель НЕ вызывается второй раз;
// - react: outbox type=reaction c message_id первого события → DeliveryLoop
//   доставляет через setMessageReaction;
// - 1.9: дневной бюджет модельных вызовов — SkipWorkError без вызова модели,
//   резерв operation_id model:{work_item_id}:{attempt};
// - 1.10: notes сверх 2000 симв. — весь ответ отклонён схемой; validateDeltas
//   в обработчике не обойдён.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool, PoolClient } from "pg";
import { makeTurnHandler } from "./turn-handler.js";
import { DEFAULT_PERSONA } from "./persona.js";
import { MoodRepo } from "./mood.js";
import { RelationshipRepo } from "./relationships.js";
import { OpenAICompatModelAdapter, ModelError, type CharacterModel, type ModelTransport, type ModelTurnContext } from "./model-adapter.js";
import type { TurnResult } from "./turn-result.js";
import { MemoryRepo } from "../memory/memory-repo.js";
import { IntentionRepo } from "../background/intentions.js";
import { chatScope } from "../memory/scope.js";
import { OutboxRepo } from "../telegram/outbox.js";
import { DeliveryLoop } from "../telegram/delivery.js";
import type { TelegramApi, SendMessageResult, TelegramUpdate } from "../telegram/api.js";
import { SkipWorkError } from "../runtime/worker.js";
import { BudgetRepo } from "../runtime/budgets.js";
import { withTransaction } from "../runtime/transactions.js";
import { DeltaError } from "../runtime/deltas.js";
import { WorkQueueRepo, type WorkItem } from "../runtime/work-queue.js";
import { cleanMemoryTables, createTestPool } from "../test-support/db.js";
import { cleanTelegramTables, getOutboxByTurn, silentLogger } from "../test-support/telegram-db.js";

let pool: Pool;
let outbox: OutboxRepo;
let workRepo: WorkQueueRepo;
let memory: MemoryRepo;
let nextUpdateId = 8_000;
let nextMessageId = 60_000;

beforeEach(async () => {
  pool = createTestPool();
  outbox = new OutboxRepo(pool);
  workRepo = new WorkQueueRepo(pool);
  memory = new MemoryRepo(pool);
  await cleanTelegramTables(pool);
  await cleanMemoryTables(pool); // каскадом чистит relationships
  await pool.query("DELETE FROM mood_state");
  nextUpdateId = 8_000;
  nextMessageId = 60_000;
});

afterEach(async () => {
  // не оставляем соседям по базе singleton-mood и отношения
  await pool.query("DELETE FROM mood_state").catch(() => undefined);
  await pool.query("DELETE FROM relationships").catch(() => undefined);
  await cleanMemoryTables(pool).catch(() => undefined);
  await pool.end();
});

// ---------- помощники ----------

/** События с raw_update.message.message_id (цель реакции) и actor_name. */
async function makeTurn(
  chatId: string,
  texts: string[],
  options: { actorName?: string | null } = {},
): Promise<{ turnId: string; item: WorkItem }> {
  const turn = await pool.query<{ id: string }>(
    "INSERT INTO turns (chat_id) VALUES ($1) RETURNING id::text",
    [chatId],
  );
  const turnId = turn.rows[0]!.id;
  for (const text of texts) {
    await pool.query(
      `INSERT INTO social_events
         (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, turn_id)
       VALUES ($1, $2, 'private', '42', $3, 'message', $4, $5, 'pending', $6)`,
      [
        nextUpdateId++,
        chatId,
        options.actorName ?? null,
        text,
        JSON.stringify({ message: { message_id: nextMessageId++, text } }),
        turnId,
      ],
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

/** Модель-стаб со счётчиком вызовов: возвращает заданный результат как есть. */
function countingModel(
  partial: Partial<TurnResult> & Pick<TurnResult, "decision">,
): { model: CharacterModel; calls: () => number } {
  // нормализация как у TurnResultSchema.parse: дефолты пустых списков
  const result: TurnResult = { messages: [], notes: [], memory: [], intentions: [], ...partial };
  let calls = 0;
  return {
    model: {
      async runTurn(): Promise<TurnResult> {
        calls++;
        return result;
      },
    },
    calls: () => calls,
  };
}

/** Адаптер с заглушкой транспорта: отвечает заданными tool-аргументами. */
function adapterModel(
  toolArguments: unknown,
  transportCalls: unknown[],
  options: { maxRetries?: number } = {},
): OpenAICompatModelAdapter {
  const transport: ModelTransport = async (_url, init) => {
    transportCalls.push(init.body);
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
  return new OpenAICompatModelAdapter({
    endpoint: "https://model.example/v1",
    apiKey: "key",
    model: "test-model",
    transport,
    maxRetries: options.maxRetries ?? 1,
  });
}

/** Реальный сбой применения: первая же постановка outbox падает, tx откатывается. */
class FlakyOutbox extends OutboxRepo {
  public failNextEnqueue = false;

  override async enqueueMessages(
    client: PoolClient,
    turnId: string,
    chatId: string,
    texts: string[],
    bubbleDelayMs = 800,
  ): Promise<number> {
    if (this.failNextEnqueue) {
      this.failNextEnqueue = false;
      throw new Error("инъекция сбоя: соединение с БД потеряно при применении");
    }
    return super.enqueueMessages(client, turnId, chatId, texts, bubbleDelayMs);
  }
}

/** Свой фейковый Telegram API: записывает и sendMessage, и setMessageReaction. */
class FakeApi implements TelegramApi {
  public readonly sent: Array<{ chatId: string; text: string }> = [];
  public readonly reactions: Array<{ chatId: string; messageId: string; emoji: string }> = [];

  async getUpdates(_offset: number, _timeoutSec: number, _signal: AbortSignal): Promise<TelegramUpdate[]> {
    return [];
  }

  async sendMessage(chatId: string, text: string, _signal: AbortSignal): Promise<SendMessageResult> {
    this.sent.push({ chatId, text });
    return { messageId: String(500_000 + this.sent.length) };
  }

  async setMessageReaction(chatId: string, messageId: string, emoji: string, _signal: AbortSignal): Promise<void> {
    this.reactions.push({ chatId, messageId, emoji });
  }
}

function handlerWith(
  model: CharacterModel | null,
  options: { outboxRepo?: OutboxRepo; modelBudget?: BudgetRepo | null; dailyLimit?: number } = {},
) {
  return makeTurnHandler({
    pool,
    model,
    outbox: options.outboxRepo ?? outbox,
    memory,
    intentions: new IntentionRepo(pool),
    persona: PERSONA_STUB,
    mood: new MoodRepo(pool),
    relationships: new RelationshipRepo(pool),
    workQueue: workRepo,
    modelBudget: options.modelBudget ?? null,
    modelDailyCallsLimit: options.dailyLimit ?? 100,
    logger: silentLogger,
  });
}

const SIGNAL = () => new AbortController().signal;

async function moodRow() {
  const result = await pool.query<{ valence: string; arousal: string; social_energy: string }>(
    "SELECT valence, arousal, social_energy FROM mood_state WHERE id = 1",
  );
  const row = result.rows[0];
  return row
    ? { valence: Number(row.valence), arousal: Number(row.arousal), socialEnergy: Number(row.social_energy) }
    : null;
}

async function notesOf(workItemId: string) {
  const result = await pool.query<{ text: string; op_index: number }>(
    "SELECT text, op_index FROM state_notes WHERE work_item_id = $1 ORDER BY op_index",
    [workItemId],
  );
  return result.rows.map((row) => row.text);
}

// ---------- состояние персонажа применяется ходом (§7.2–7.3) ----------

describe("ход применяет mood_delta с основанием (§7.2, чек-лист 4.2)", () => {
  it("дельта сверх лимита сжата до +0.3, mood_state = baseline + кламп, заметка хранит основание", async () => {
    const { item } = await makeTurn("500", ["рассказал смешную шутку"]);
    const { model } = countingModel({
      decision: "answer",
      messages: ["ха-ха"],
      notes: [],
      mood_delta: { valence: 0.9, arousal: 0.2, reason: "смех от шутки" },
    });

    await handlerWith(model)(item, SIGNAL());

    // baseline {0.1, 0.4, 0.6}; valence +0.9 сжат до +0.3, arousal +0.2 как есть
    const mood = await moodRow();
    expect(mood).toEqual({ valence: 0.4, arousal: 0.6, socialEnergy: 0.6 });

    const notes = await notesOf(item.id);
    const moodNote = notes.find((text) => text.startsWith("mood:"));
    expect(moodNote).toBeDefined();
    expect(moodNote).toContain('"valence":0.3'); // в заметке — сжатая дельта, не 0.9
    expect(moodNote).toContain("смех от шутки"); // основание (§7.2)
  });
});

describe("ход применяет relationship_delta и создаёт person-entity (§7.3, чек-лист 4.3)", () => {
  it("person-entity из actor_name события; affinity/trust сжаты до ±0.1; familiarity +0.05; заметка с основанием", async () => {
    const { item } = await makeTurn("501", ["привет, я Алиса"], { actorName: "Алиса" });
    const { model } = countingModel({
      decision: "answer",
      messages: ["привет, Алиса"],
      notes: [],
      relationship_delta: { affinity: 0.5, trust: 0.5, reason: "тёплое знакомство" },
    });

    await handlerWith(model)(item, SIGNAL());

    // person-entity в scope чата с именем из события (actor_name, не user:42)
    const entities = await pool.query<{ id: string; kind: string; name: string; scope_id: string }>(
      "SELECT id::text, kind, canonical_name AS name, scope_id FROM entities WHERE kind = 'person'",
    );
    expect(entities.rows).toEqual([
      { id: expect.any(String) as string, kind: "person", name: "Алиса", scope_id: chatScope("501") },
    ]);
    const personId = entities.rows[0]!.id;

    const relationship = await new RelationshipRepo(pool).read(personId, chatScope("501"));
    expect(relationship.familiarity).toBeCloseTo(0.05, 10); // runtime-правило значимого хода
    expect(relationship.affinity).toBeCloseTo(0.1, 10); // +0.5 сжат до +0.1
    expect(relationship.trust).toBeCloseTo(0.2 + 0.1, 10); // дефолт 0.2 + 0.1

    const notes = await notesOf(item.id);
    const relNote = notes.find((text) => text.startsWith("отношения:"));
    expect(relNote).toBeDefined();
    expect(relNote).toContain("тёплое знакомство"); // основание (§7.3)
  });

  it("второй ход того же собеседника: familiarity накапливается, affinity двигается ещё на 0.1", async () => {
    const first = await makeTurn("502", ["сообщение раз"], { actorName: "Борис" });
    const delta = {
      decision: "answer",
      messages: ["ок"],
      notes: [],
      relationship_delta: { affinity: 0.5, reason: "приятный тон" },
    } satisfies Partial<TurnResult> & Pick<TurnResult, "decision">;
    await handlerWith(countingModel(delta).model)(first.item, SIGNAL());

    const second = await makeTurn("502", ["сообщение два"], { actorName: "Борис" });
    await handlerWith(countingModel(delta).model)(second.item, SIGNAL());

    const person = await pool.query<{ id: string }>(
      "SELECT id::text FROM entities WHERE kind = 'person' AND canonical_name = 'Борис'",
    );
    const relationship = await new RelationshipRepo(pool).read(person.rows[0]!.id, chatScope("502"));
    expect(relationship.familiarity).toBeCloseTo(0.1, 10); // 0.05 + 0.05
    expect(relationship.affinity).toBeCloseTo(0.2, 10); // 0.1 + 0.1, лимит — за ход
  });

  it("без actor_name person-entity = user:{actor_id}", async () => {
    const { item } = await makeTurn("503", ["анонимное сообщение"]);
    const { model } = countingModel({ decision: "answer", messages: ["ок"], notes: [] });

    await handlerWith(model)(item, SIGNAL());

    const entities = await pool.query<{ name: string }>(
      "SELECT canonical_name AS name FROM entities WHERE kind = 'person'",
    );
    expect(entities.rows).toEqual([{ name: "user:42" }]);
  });
});

// ---------- result_ready: результат переживает сбой применения (§12.1–12.2, 2.7) ----------

describe("result_ready: сбой применения до commit — повтор применяет сохранённый результат (2.7)", () => {
  it("модель вызвана ровно один раз; после сбоя статус result_ready, повтор применяет без LLM", async () => {
    const flaky = new FlakyOutbox(pool);
    const result: TurnResult = {
      decision: "answer",
      messages: ["сохранённый ответ"],
      notes: ["заметка хода"],
      memory: [],
      intentions: [],
      reaction: undefined,
      mood_delta: undefined,
      relationship_delta: undefined,
      reason: undefined,
    };
    const { model, calls } = countingModel(result);
    const { turnId, item } = await makeTurn("504", ["вопрос"]);

    // --- первый прогон: результат сохранён, применение падает целиком ---
    flaky.failNextEnqueue = true;
    await expect(handlerWith(model, { outboxRepo: flaky })(item, SIGNAL())).rejects.toThrow("инъекция сбоя");

    const turn = await pool.query<{ status: string; decision: string | null }>(
      "SELECT status, validated_result->>'decision' AS decision FROM turns WHERE id = $1",
      [turnId],
    );
    expect(turn.rows[0]!.status).toBe("result_ready"); // §12.1: валидированный результат сохранён ДО применения
    expect(turn.rows[0]!.decision).toBe("answer");
    // применение откатилось целиком: ничего не применено
    expect((await getOutboxByTurn(pool, turnId)).length).toBe(0);
    expect(await notesOf(item.id)).toEqual([]);
    const events = await pool.query<{ status: string }>(
      "SELECT status FROM social_events WHERE turn_id = $1",
      [turnId],
    );
    expect(events.rows.every((row) => row.status === "pending")).toBe(true);
    expect(calls()).toBe(1);

    // --- «worker» вернул работу в очередь после ошибки; повторный захват ---
    await workRepo.requeue(item.id);
    const retry = await workRepo.claimNext();
    expect(retry!.id).toBe(item.id);
    expect(retry!.attempts).toBe(2);

    await handlerWith(model, { outboxRepo: flaky })(retry!, SIGNAL());

    // модель НЕ вызывалась второй раз — применён сохранённый результат
    expect(calls()).toBe(1);
    const after = await pool.query<{ status: string }>("SELECT status FROM turns WHERE id = $1", [turnId]);
    expect(after.rows[0]!.status).toBe("applied");
    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows.map((row) => row.payload.text)).toEqual(["сохранённый ответ"]);
    expect(await notesOf(item.id)).toEqual(["заметка хода"]);
    const eventsAfter = await pool.query<{ status: string }>(
      "SELECT status FROM social_events WHERE turn_id = $1",
      [turnId],
    );
    expect(eventsAfter.rows.every((row) => row.status === "processed")).toBe(true);
  });
});

// ---------- react: реакция как единица доставки (§11.2, §13) ----------

describe("react: outbox type=reaction и доставка через setMessageReaction (§13.2)", () => {
  it("реакция ставится на message_id ПЕРВОГО события turn, сообщение не отправляется", async () => {
    const firstMessageId = nextMessageId; // makeTurn выдаст message_id по возрастанию
    const { turnId, item } = await makeTurn("505", ["первое сообщение", "второе сообщение"]);
    const { model } = countingModel({ decision: "react", messages: [], notes: [], reaction: "🔥" });

    await handlerWith(model)(item, SIGNAL());

    const rows = await getOutboxByTurn(pool, turnId);
    expect(rows).toHaveLength(1);
    expect(rows[0]!.position).toBe(0);
    expect(rows[0]!.payload).toEqual({
      type: "reaction",
      emoji: "🔥",
      message_id: String(firstMessageId),
    });
    expect(rows[0]!.status).toBe("pending");

    // DeliveryLoop доставляет реакцию через setMessageReaction (свой фейковый API)
    const api = new FakeApi();
    const loop = new DeliveryLoop({ pool, repo: outbox, api, logger: silentLogger });
    await loop.tick();

    expect(api.reactions).toEqual([{ chatId: "505", messageId: String(firstMessageId), emoji: "🔥" }]);
    expect(api.sent).toEqual([]); // react не отправляет сообщений
    // §13.1: после подтверждённого успеха строка sent (не unknown — отправка была)
    const delivered = await getOutboxByTurn(pool, turnId);
    expect(delivered[0]!.status).toBe("sent");
  });
});

// ---------- 1.9: дневной бюджет модельных вызовов ----------

describe("история контекста: только ОТПРАВЛЕННЫЕ ответы (§13.5)", () => {
  it("pending-ответ не часть разговора; после sent появляется как assistant", async () => {
    // ход 1: ответ попал в outbox, но ещё не отправлен (pending)
    const first = await makeTurn("510", ["вопрос первый"]);
    await handlerWith(
      countingModel({ decision: "answer", messages: ["мой ответ"], notes: [] }).model,
    )(first.item, SIGNAL());

    // ход 2 того же чата: pending-ответ НЕ подставляется как свершившийся факт
    const secondCtx: ModelTurnContext[] = [];
    const capture = (partial: Partial<TurnResult> & Pick<TurnResult, "decision">): CharacterModel => {
      const result: TurnResult = { messages: [], notes: [], memory: [], intentions: [], ...partial };
      return {
        async runTurn(input): Promise<TurnResult> {
          secondCtx.push(input.context);
          return result;
        },
      };
    };
    const second = await makeTurn("510", ["вопрос второй"]);
    await handlerWith(capture({ decision: "answer", messages: ["ок"], notes: [] }))(second.item, SIGNAL());
    expect(secondCtx).toHaveLength(1);
    expect(secondCtx[0]!.messages).toEqual([{ role: "user", content: "вопрос второй" }]);

    // доставка подтверждена → ответ стал фактом разговора
    await pool.query("UPDATE delivery_outbox SET status = 'sent' WHERE turn_id = $1", [first.turnId]);
    const third = await makeTurn("510", ["вопрос третий"]);
    await handlerWith(capture({ decision: "answer", messages: ["ок"], notes: [] }))(third.item, SIGNAL());
    expect(secondCtx).toHaveLength(2);
    expect(secondCtx[1]!.messages).toEqual([
      { role: "assistant", content: "мой ответ" },
      { role: "user", content: "вопрос третий" },
    ]);
  });
});

describe("дневной бюджет модели (§16, чек-лист 1.9)", () => {
  it("MODEL_DAILY_CALLS_LIMIT=1: первый ход проходит с резервом model:{id}:{attempt}, второй — SkipWorkError без вызова", async () => {
    const budget = new BudgetRepo(pool);
    const { model, calls } = countingModel({ decision: "answer", messages: ["ответ"], notes: [] });
    const handler = handlerWith(model, { modelBudget: budget, dailyLimit: 1 });

    const first = await makeTurn("600", ["первый вопрос"]);
    await handler(first.item, SIGNAL());
    expect(calls()).toBe(1);

    // резерв: детерминированный operation_id вида model:{work_item_id}:{attempt}
    const reservations = await pool.query<{ operation_id: string }>(
      "SELECT operation_id FROM budget_reservations ORDER BY id",
    );
    expect(reservations.rows).toEqual([{ operation_id: `model:${first.item.id}:1` }]);
    expect(await budget.reservedToday()).toBe(1);

    // второй ход: бюджет исчерпан — осознанный пропуск ДО вызова транспорта
    const second = await makeTurn("601", ["второй вопрос"]);
    const rejection = await handler(second.item, SIGNAL()).catch((error: unknown) => error);
    expect(rejection).toBeInstanceOf(SkipWorkError);
    expect((rejection as Error).message).toContain("бюджет");
    expect(calls()).toBe(1); // модель не вызывалась

    const turn2 = await pool.query<{ status: string }>("SELECT status FROM turns WHERE id = $1", [
      second.turnId,
    ]);
    expect(turn2.rows[0]!.status).toBe("created");
    expect((await getOutboxByTurn(pool, second.turnId)).length).toBe(0);
  });
});

// ---------- 1.10: лимиты объёма на пути выполнения ----------

describe("лимиты строк ответа (§11.3, чек-лист 1.10)", () => {
  it("note 2001 симв.: схема отклоняет ВЕСЬ ответ, адаптер ретраится и сдаётся — ничего не применяется", async () => {
    const { turnId, item } = await makeTurn("506", ["вопрос"]);
    const transportCalls: unknown[] = [];
    const adapter = adapterModel({ decision: "ignore", notes: ["н".repeat(2001)] }, transportCalls);

    await expect(handlerWith(adapter)(item, SIGNAL())).rejects.toBeInstanceOf(ModelError);
    expect(transportCalls).toHaveLength(2); // 1 попытка + 1 retry (maxRetries=1)

    const turn = await pool.query<{ status: string }>("SELECT status FROM turns WHERE id = $1", [turnId]);
    expect(turn.rows[0]!.status).toBe("created"); // невалидное не сохранилось даже как result_ready
    expect(await notesOf(item.id)).toEqual([]);
    expect((await getOutboxByTurn(pool, turnId)).length).toBe(0);
    expect(await moodRow()).toBeNull(); // mood_delta (если бы был) не применён
  });

  it("пустая note невозможна после схемы: notes [''] отклоняются так же", async () => {
    const { turnId, item } = await makeTurn("507", ["вопрос"]);
    const transportCalls: unknown[] = [];
    const adapter = adapterModel({ decision: "ignore", notes: [""] }, transportCalls);

    await expect(handlerWith(adapter)(item, SIGNAL())).rejects.toBeInstanceOf(ModelError);
    expect(await notesOf(item.id)).toEqual([]);
    expect((await getOutboxByTurn(pool, turnId)).length).toBe(0);
  });

  it("validateDeltas в обработчике не обойдён: note 2001 симв. минуя адаптер → DeltaError, ничего не применяется", async () => {
    const { turnId, item } = await makeTurn("508", ["вопрос"]);
    // стаб в обход адаптера: возвращает невалидную по объёму заметку
    const { model } = countingModel({
      decision: "answer",
      messages: ["ответ"],
      notes: ["н".repeat(2001)],
    });

    await expect(handlerWith(model)(item, SIGNAL())).rejects.toBeInstanceOf(DeltaError);

    // применение откатилось: заметок и доставки нет, события не обработаны
    expect(await notesOf(item.id)).toEqual([]);
    expect((await getOutboxByTurn(pool, turnId)).length).toBe(0);
    const events = await pool.query<{ status: string }>(
      "SELECT status FROM social_events WHERE turn_id = $1",
      [turnId],
    );
    expect(events.rows.every((row) => row.status === "pending")).toBe(true);
  });
  it("промпт хода содержит настроение и отношение (§7.2, §7.3: assembleContext → buildSystemPrompt)", async () => {
    // mood сдвинут заранее: baseline 0.1 → заметно приподнятое (0.6)
    const moodRepo = new MoodRepo(pool);
    await withTransaction(pool, (client) =>
      moodRepo.applyDelta(client, PERSONA_STUB.moodBaseline, { valence: 0.5 }),
    );
    const { item } = await makeTurn("700", ["как дела?"], { actorName: "Аня" });
    const captured: Array<{ system: string }> = [];
    const model: CharacterModel = {
      async runTurn(input): Promise<TurnResult> {
        captured.push({ system: input.context.system });
        return { decision: "answer", messages: ["норм"], notes: [], memory: [], intentions: [] };
      },
    };

    await handlerWith(model)(item, SIGNAL());

    expect(captured).toHaveLength(1);
    expect(captured[0]!.system).toContain("Твоё состояние сейчас: в приподнятом настроении");
    expect(captured[0]!.system).toContain("Отношение к собеседнику: почти незнаком");
  });

});
