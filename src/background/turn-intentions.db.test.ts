// Интеграционный тест (живой PostgreSQL, база dio_test).
// Этап 5 «Фон» — зарождение намерений в разговорном ходе (§10.1, §11.2, §14.1):
// - decision=defer + defer_hours → reply_later с темой последнего текста пачки;
// - intentions в TurnResult → созданы (kind/topic/reason/delay, related_turn_id);
// - дубликаты предложений (в одном ходе и между ходами) → одна строка (dedupe).

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { makeTurnHandler } from "../character/turn-handler.js";
import { DEFAULT_PERSONA } from "../character/persona.js";
import { MoodRepo } from "../character/mood.js";
import { RelationshipRepo } from "../character/relationships.js";
import type { CharacterModel } from "../character/model-adapter.js";
import type { TurnResult } from "../character/turn-result.js";
import { TurnResultSchema } from "../character/turn-result.js";
import { MemoryRepo } from "../memory/memory-repo.js";
import { IntentionRepo } from "./intentions.js";
import { OutboxRepo } from "../telegram/outbox.js";
import { WorkQueueRepo, type WorkItem } from "../runtime/work-queue.js";
import { cleanMemoryTables, createTestPool } from "../test-support/db.js";
import { cleanTelegramTables, silentLogger } from "../test-support/telegram-db.js";

let pool: Pool;
let workRepo: WorkQueueRepo;

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

beforeEach(async () => {
  pool = createTestPool();
  workRepo = new WorkQueueRepo(pool);
  await cleanTelegramTables(pool);
  await cleanMemoryTables(pool);
  await pool.query("DELETE FROM mood_state");
  await pool.query("TRUNCATE TABLE proactive_log, intentions RESTART IDENTITY CASCADE");
});

afterEach(async () => {
  await pool.query("DELETE FROM mood_state").catch(() => undefined);
  await pool.query("DELETE FROM relationships").catch(() => undefined);
  await cleanMemoryTables(pool).catch(() => undefined);
  await pool.end();
});

let nextUpdateId = 30_000;

/** Turn + события пачки + direct-работа, как это сделал бы ingest. */
async function makeTurn(
  chatId: string,
  texts: Array<string | null>,
): Promise<{ turnId: string; item: WorkItem }> {
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

function handlerWith(model: CharacterModel) {
  return makeTurnHandler({
    pool,
    model,
    outbox: new OutboxRepo(pool),
    memory: new MemoryRepo(pool),
    intentions: new IntentionRepo(pool),
    persona: PERSONA_STUB,
    mood: new MoodRepo(pool),
    relationships: new RelationshipRepo(pool),
    workQueue: workRepo,
    modelBudget: null,
    modelDailyCallsLimit: 100,
    historyMessageLimit: 1000,
    historyMaxChars: 200_000,
    logger: silentLogger,
  });
}

/** Модель-стаб: возвращает заданный TurnResult как есть. */
function stubModel(partial: Partial<TurnResult> & Pick<TurnResult, "decision">): CharacterModel {
  const result: TurnResult = { messages: [], notes: [], memory: [], intentions: [], ...partial };
  return {
    async runTurn(): Promise<TurnResult> {
      return result;
    },
  };
}

const SIGNAL = () => new AbortController().signal;

async function intentions() {
  const rows = await pool.query<
    { id: string; kind: string; topic: string; reason: string; status: string; not_before: Date; related_turn_id: string | null }
  >(
    "SELECT id::text, kind, topic, reason, status, not_before, related_turn_id::text FROM intentions ORDER BY id",
  );
  return rows.rows;
}

// ---------- defer → reply_later (§11.2) ----------

describe("defer создаёт reply_later (§11.2)", () => {
  it("тема — последний текст пачки, not_before = now + defer_hours, ответ не отправляется", async () => {
    const { turnId, item } = await makeTurn("4001", ["вопрос про дачу", "точнее про сарай на даче"]);

    await handlerWith(stubModel({ decision: "defer", defer_hours: 1.5 }))(item, SIGNAL());

    const rows = await intentions();
    expect(rows).toHaveLength(1);
    const replyLater = rows[0]!;
    expect(replyLater.kind).toBe("reply_later");
    expect(replyLater.topic).toBe("точнее про сарай на даче"); // последний текст (ORDER BY id DESC)
    expect(replyLater.reason).toContain("defer");
    expect(replyLater.status).toBe("pending");
    expect(replyLater.related_turn_id).toBe(turnId);
    const inHours = (replyLater.not_before.getTime() - Date.now()) / 3_600_000;
    expect(inHours).toBeGreaterThan(1.4);
    expect(inHours).toBeLessThan(1.6);
    // defer не отправляет ничего (§11.2: ignore/defer без сообщений)
    const outbox = await pool.query<{ c: number }>(
      "SELECT count(*)::int AS c FROM delivery_outbox WHERE turn_id = $1",
      [turnId],
    );
    expect(outbox.rows[0]!.c).toBe(0);
    const turn = await pool.query<{ status: string }>("SELECT status FROM turns WHERE id = $1", [turnId]);
    expect(turn.rows[0]!.status).toBe("applied");
    const events = await pool.query<{ status: string }>(
      "SELECT status FROM social_events WHERE turn_id = $1",
      [turnId],
    );
    expect(events.rows.every((row) => row.status === "processed")).toBe(true);
  });

  it("defer без текстов в пачке — тема-фолбэк «отложенный ответ»", async () => {
    const { item } = await makeTurn("4002", [null]); // событие без текста (text IS NULL)

    await handlerWith(stubModel({ decision: "defer", defer_hours: 0.1 }))(item, SIGNAL());

    const rows = await intentions();
    expect(rows).toHaveLength(1);
    expect(rows[0]!.topic).toBe("отложенный ответ");
  });
});

// ---------- intentions из TurnResult (§10.1, §14.1) ----------

describe("intentions в TurnResult создаются ходом (§14.1)", () => {
  it("валидное предложение → намерение pending с related_turn_id и not_before = now + delay", async () => {
    const { turnId, item } = await makeTurn("4003", ["сдал первый экзамен?"]);
    const proposal = { kind: "check_in", topic: "экзамен", reason: "обещал спросить", delay_hours: 0.1 } as const;

    await handlerWith(
      stubModel({ decision: "answer", messages: ["удачи!"], intentions: [proposal] }),
    )(item, SIGNAL());

    const rows = await intentions();
    expect(rows).toHaveLength(1);
    expect(rows[0]!.kind).toBe("check_in");
    expect(rows[0]!.topic).toBe("экзамен");
    expect(rows[0]!.reason).toBe("обещал спросить");
    expect(rows[0]!.status).toBe("pending");
    expect(rows[0]!.related_turn_id).toBe(turnId);
    const inMinutes = (rows[0]!.not_before.getTime() - Date.now()) / 60_000;
    expect(inMinutes).toBeGreaterThan(5); // 0.1 ч = 6 минут
    expect(inMinutes).toBeLessThan(7);
    // ответ при этом доставляется как обычно
    const outbox = await pool.query<{ c: number }>(
      "SELECT count(*)::int AS c FROM delivery_outbox WHERE turn_id = $1",
      [turnId],
    );
    expect(outbox.rows[0]!.c).toBe(1);
  });

  it("два одинаковых предложения в одном ходе → одна строка (dedupe на create)", async () => {
    const { item } = await makeTurn("4004", ["про горы поговорим потом"]);
    const proposal = { kind: "explore", topic: "горы", reason: "интересно", delay_hours: 1 } as const;

    await handlerWith(
      stubModel({ decision: "answer", messages: ["ок"], intentions: [proposal, { ...proposal, delay_hours: 2 }] }),
    )(item, SIGNAL());

    const rows = await intentions();
    expect(rows).toHaveLength(1); // второй create → null, дубль не создан
  });

  it("то же предложение в следующем ходе — по-прежнему одна строка", async () => {
    const proposal = { kind: "check_in", topic: "курс лечения", reason: "важно", delay_hours: 3 } as const;
    const first = await makeTurn("4005", ["хожу на процедуры"]);
    await handlerWith(stubModel({ decision: "answer", messages: ["держись"], intentions: [proposal] }))(
      first.item,
      SIGNAL(),
    );
    const second = await makeTurn("4005", ["процедуры продолжаются"]);
    await handlerWith(stubModel({ decision: "answer", messages: ["молодец"], intentions: [proposal] }))(
      second.item,
      SIGNAL(),
    );

    const rows = await intentions();
    expect(rows).toHaveLength(1);
    expect(rows[0]!.related_turn_id).toBe(first.turnId); // осталось первое вхождение
  });

  it("answer + defer_hours в одном результате не создаёт reply_later повторно: defer-ветка работает только при decision=defer", async () => {
    const { item } = await makeTurn("4006", ["текст сообщения"]);

    await handlerWith(stubModel({ decision: "answer", messages: ["отвечу сейчас"], defer_hours: 1 }))(
      item,
      SIGNAL(),
    );

    const rows = await intentions();
    expect(rows).toHaveLength(0); // defer_hours при answer игнорируется созданием reply_later
  });
  it("defer без defer_hours: схема отвергает весь ответ, намерение не создаётся (§11.2, желательная доработка критика)", () => {
    const parsed = TurnResultSchema.safeParse({ decision: "defer", messages: [], notes: [] });
    expect(parsed.success).toBe(false);
    if (!parsed.success) {
      const paths = parsed.error.issues.map((issue) => issue.path.join("."));
      expect(paths).toContain("defer_hours");
    }
  });

});
