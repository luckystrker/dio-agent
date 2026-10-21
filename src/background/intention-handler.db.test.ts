// Интеграционный тест (живой PostgreSQL, база dio_test).
// Этап 5 «Фон» — исполнение намерений (§5.2, §14–§16) со стаб-моделью:
// - §5.2: pending direct/group в work_items → хендлер откладывает (requeue 5с),
//   намерение НЕ отменено, модель не вызывалась;
// - свежий контекст-гейт §14.3: событие чата новее created_at → отмена;
// - proactive-политика §14.3: 2 за сегодня / пауза <4ч / группа → отмена до модели;
// - инициатива (answer): инициативный turn applied + outbox + proactive_log +
//   initiative_topics + память кандидатов + общий резерв бюджета (operation_id);
// - exploration §15/§16: только память (outbox пуст), answer → share-намерение
//   ~10 мин, дневной лимит 2, счётчик — только completed за сегодня;
// - guard: терминальный статус закрывает работу; повтор после сбоя применения —
//   фактическое поведение задокументировано (см. блок «сбой применения»).

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { makeIntentionHandler } from "./intention-handler.js";
import { IntentionRepo } from "./intentions.js";
import { MoodRepo } from "../character/mood.js";
import type { IntentionKind } from "./intention-proposals.js";
import { BudgetRepo } from "../runtime/budgets.js";
import { withTransaction } from "../runtime/transactions.js";
import { WorkQueueRepo, type WorkItem } from "../runtime/work-queue.js";
import { MemoryRepo } from "../memory/memory-repo.js";
import { chatScope } from "../memory/scope.js";
import { InitiativeRepo } from "../character/initiative.js";
import { DEFAULT_PERSONA, type Persona } from "../character/persona.js";
import type { CharacterModel } from "../character/model-adapter.js";
import type { TurnResult } from "../character/turn-result.js";
import { OutboxRepo } from "../telegram/outbox.js";
import { cleanMemoryTables, createTestPool, waitFor } from "../test-support/db.js";
import { cleanTelegramTables, silentLogger } from "../test-support/telegram-db.js";

let pool: Pool;
let repo: IntentionRepo;
let workRepo: WorkQueueRepo;
let memory: MemoryRepo;
let outbox: OutboxRepo;
let initiative: InitiativeRepo;
let nextUpdateId = 20_000;

const PERSONA_STUB: Persona = {
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
  repo = new IntentionRepo(pool);
  workRepo = new WorkQueueRepo(pool);
  memory = new MemoryRepo(pool);
  outbox = new OutboxRepo(pool);
  initiative = new InitiativeRepo(pool);
  await cleanTelegramTables(pool); // turns, social_events, outbox, work_items, notes, резервы
  await pool.query("DELETE FROM initiative_topics");
  await cleanMemoryTables(pool); // каскадом чистит initiative_topics/relationships
  await pool.query("TRUNCATE TABLE proactive_log, intentions RESTART IDENTITY CASCADE");
  nextUpdateId = 20_000;
});

afterEach(async () => {
  await pool.end();
});

// ---------- помощники ----------

/** Событие чата (как после ingest): occurred_at по умолчанию now(). */
async function seedChatEvent(
  chatId: string,
  chatType: "private" | "group",
  occurredAtSql = "now()",
): Promise<void> {
  await pool.query(
    `INSERT INTO social_events
       (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status, occurred_at)
     VALUES ($1, $2, $3, '42', 'message', 'текст', '{}', 'pending', ${occurredAtSql})`,
    [nextUpdateId++, chatId, chatType],
  );
}

interface QueueOptions {
  delayHours?: number;
  /** сдвинуть created_at в прошлое — для сценария «чат ушёл вперёд» */
  backdateHours?: number;
  /** просрочить ПОСЛЕ захвата — сценарий «истёк между захватом и исполнением» */
  expireHoursAgo?: number;
}

/** Намерение сразу due: создано, переведено в queued, работа захвачена. */
async function queueIntention(
  kind: IntentionKind,
  topic: string,
  chatId: string,
  options: QueueOptions = {},
): Promise<{ intentionId: string; item: WorkItem }> {
  const created = await withTransaction(pool, (client) =>
    repo.create(client, {
      kind,
      topic,
      reason: "тестовое намерение",
      targetChatId: chatId,
      delayHours: options.delayHours ?? -0.01,
    }),
  );
  expect(created).not.toBeNull();
  const intentionId = created!.id;
  await pool.query("UPDATE intentions SET not_before = now() - interval '5 minutes' WHERE id = $1", [
    intentionId,
  ]);
  if (options.backdateHours !== undefined) {
    await pool.query("UPDATE intentions SET created_at = now() - ($2::int * interval '1 hour') WHERE id = $1", [
      intentionId,
      options.backdateHours,
    ]);
  }
  // захват ДО искусственной просрочки: claimDue сам не берёт истёкшие (§14.2),
  // хендлер проверяет expires_at повторно — в том числе между захватом и исполнением
  expect(await repo.claimDue(1)).toBe(1);
  if (options.expireHoursAgo !== undefined) {
    await pool.query("UPDATE intentions SET expires_at = now() - ($2::int * interval '1 hour') WHERE id = $1", [
      intentionId,
      options.expireHoursAgo,
    ]);
  }
  const item = await workRepo.claimNext();
  expect(item).not.toBeNull();
  expect(item!.kind).toBe("intention");
  return { intentionId, item: item! };
}

/** Модель-стаб со счётчиком вызовов (как в turn-stage4). */
function countingModel(partial: Partial<TurnResult> & Pick<TurnResult, "decision">): {
  model: CharacterModel;
  calls: () => number;
} {
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

function handlerWith(
  model: CharacterModel,
  options: { intentionsRepo?: IntentionRepo; outboxRepo?: OutboxRepo; modelBudget?: BudgetRepo | null } = {},
) {
  return makeIntentionHandler({
    pool,
    model,
    mood: new MoodRepo(pool),
    memory,
    persona: PERSONA_STUB,
    outbox: options.outboxRepo ?? outbox,
    intentions: options.intentionsRepo ?? repo,
    initiative,
    workQueue: workRepo,
    modelBudget: options.modelBudget ?? null,
    modelDailyCallsLimit: 100,
    logger: silentLogger,
  });
}

const SIGNAL = () => new AbortController().signal;

async function scalar(query: string, params: unknown[] = []): Promise<number> {
  const result = await pool.query<{ c: number }>(`SELECT count(*)::int AS c ${query}`, params);
  return result.rows[0]!.c;
}

async function intentionRow(id: string) {
  return repo.byId(id);
}

// ---------- §5.2: прямой разговор откладывает фон ----------

describe("намерение откладывается при прямом обращении (§5.2)", () => {
  it("pending direct в очереди → requeue с next_attempt_at в будущем, намерение живо, модель не звалась", async () => {
    const { intentionId, item } = await queueIntention("check_in", "ремонт", "3001");
    // после захвата intention-работы в очереди появляется прямое обращение
    await workRepo.enqueue("direct", { turnId: "t", chatId: "3001" });
    const { model, calls } = countingModel({ decision: "answer", messages: ["привет"] });

    await handlerWith(model)(item, SIGNAL());

    expect(calls()).toBe(0);
    const work = await workRepo.claimNext(); // работа не done: вернулась в pending
    expect(work).not.toBeNull();
    // прямое обращение (priority 50) перехватывает захват раньше отложенного (30)
    expect(work!.kind).toBe("direct");
    const paused = await pool.query<{ next_attempt_at: Date }>(
      "SELECT next_attempt_at FROM work_items WHERE id = $1",
      [item.id],
    );
    expect(new Date(paused.rows[0]!.next_attempt_at).getTime()).toBeGreaterThan(Date.now());
    const intention = await intentionRow(intentionId);
    expect(intention!.status).toBe("queued"); // НЕ отменено и НЕ running
    expect(await scalar("FROM delivery_outbox")).toBe(0);
  });

  it("pending group-работа тоже откладывает намерение", async () => {
    const { intentionId, item } = await queueIntention("share", "статья", "3002");
    await workRepo.enqueue("group", { turnId: "t", chatId: "3002" });
    const { model, calls } = countingModel({ decision: "answer", messages: ["вот"] });

    await handlerWith(model)(item, SIGNAL());

    expect(calls()).toBe(0);
    expect((await intentionRow(intentionId))!.status).toBe("queued");
    const status = await pool.query<{ status: string; next_attempt_at: Date | null }>(
      "SELECT status, next_attempt_at FROM work_items WHERE id = $1",
      [item.id],
    );
    expect(status.rows[0]!.status).toBe("pending");
    expect(status.rows[0]!.next_attempt_at).not.toBeNull();
  });
});

// ---------- просрочка и свежий контекст-гейт ----------

describe("истёкший срок и свежий контекст-гейт (§14.1, §14.3)", () => {
  it("expires_at в прошлом → честная отмена, модель не вызывалась", async () => {
    const { intentionId, item } = await queueIntention("check_in", "ремонт", "3101", {
      expireHoursAgo: 1,
    });
    const { model, calls } = countingModel({ decision: "answer", messages: ["привет"] });

    await handlerWith(model)(item, SIGNAL());

    const intention = await intentionRow(intentionId);
    expect(intention!.status).toBe("cancelled");
    expect(intention!.reason).toContain("expiresAt");
    expect(calls()).toBe(0);
    expect(await scalar("FROM work_items WHERE id = $1 AND status = 'done'", [item.id])).toBe(1);
    expect(await scalar("FROM delivery_outbox")).toBe(0);
  });

  it("событие чата новее created_at → отмена «чат ушёл вперёд», без модели и отправки", async () => {
    await seedChatEvent("3102", "private"); // старый контекст — основа намерения
    const { intentionId, item } = await queueIntention("share", "видео про горы", "3102", {
      backdateHours: 1,
    });
    await seedChatEvent("3102", "private"); // собеседник писал ПОСЛЕ замысла
    const { model, calls } = countingModel({ decision: "answer", messages: ["смотри"] });

    await handlerWith(model)(item, SIGNAL());

    const intention = await intentionRow(intentionId);
    expect(intention!.status).toBe("cancelled");
    expect(intention!.reason).toContain("чат ушёл вперёд");
    expect(calls()).toBe(0);
    expect(await scalar("FROM turns")).toBe(0);
    expect(await scalar("FROM delivery_outbox")).toBe(0);
  });
});

// ---------- guard статусов ----------

describe("guard исполнения (§12.2)", () => {
  it("терминальный статус (completed) — работа закрывается без модели", async () => {
    const { intentionId, item } = await queueIntention("check_in", "тема", "3103");
    await repo.complete(intentionId);
    const { model, calls } = countingModel({ decision: "answer", messages: ["x"] });

    await handlerWith(model)(item, SIGNAL());

    expect(calls()).toBe(0);
    expect((await intentionRow(intentionId))!.status).toBe("completed");
    expect(await scalar("FROM work_items WHERE id = $1 AND status = 'done'", [item.id])).toBe(1);
  });
});

// ---------- proactive-политика §14.3 ----------

describe("проактивная политика до вызова модели (§14.3)", () => {
  it("2 отправки за сегодня → отмена, модель не вызывалась, журнал не вырос", async () => {
    await seedChatEvent("3201", "private");
    await pool.query(
      "INSERT INTO proactive_log (chat_id, sent_at) VALUES (3201, now()), (3201, now() - interval '5 hours')",
    );
    const { intentionId, item } = await queueIntention("share", "находка", "3201");
    const { model, calls } = countingModel({ decision: "answer", messages: ["вот"] });

    await handlerWith(model)(item, SIGNAL());

    const intention = await intentionRow(intentionId);
    expect(intention!.status).toBe("cancelled");
    expect(intention!.reason).toContain("§14.3");
    expect(calls()).toBe(0);
    expect(await scalar("FROM proactive_log")).toBe(2);
  });

  it("последняя отправка меньше 4 часов назад → отмена", async () => {
    await seedChatEvent("3202", "private");
    await pool.query(
      "INSERT INTO proactive_log (chat_id, sent_at) VALUES (3202, now() - interval '1 hour')",
    );
    const { intentionId, item } = await queueIntention("check_in", "как дела", "3202");
    const { model, calls } = countingModel({ decision: "answer", messages: ["как ты"] });

    await handlerWith(model)(item, SIGNAL());

    expect((await intentionRow(intentionId))!.status).toBe("cancelled");
    expect(calls()).toBe(0);
  });

  it("групповой чат → инициатива выключена, отмена", async () => {
    await seedChatEvent("3203", "group");
    const { intentionId, item } = await queueIntention("check_in", "общее", "3203");
    const { model, calls } = countingModel({ decision: "answer", messages: ["всем привет"] });

    await handlerWith(model)(item, SIGNAL());

    const intention = await intentionRow(intentionId);
    expect(intention!.status).toBe("cancelled");
    expect(intention!.reason).toContain("§14.3");
    expect(calls()).toBe(0);
    expect(await scalar("FROM proactive_log")).toBe(0);
  });

  it("пауза 4+ часа и <2 за сегодня → инициатива доходит до модели", async () => {
    await seedChatEvent("3204", "private");
    await pool.query(
      "INSERT INTO proactive_log (chat_id, sent_at) VALUES (3204, now() - interval '5 hours')",
    );
    const { item } = await queueIntention("check_in", "экзамен", "3204");
    const { model, calls } = countingModel({ decision: "ignore" });

    await handlerWith(model)(item, SIGNAL());

    expect(calls()).toBe(1);
  });
});

// ---------- инициатива: успешная отправка ----------

describe("инициатива answer (§14.3, §4.6)", () => {
  it("turn applied + outbox + proactive_log + initiative_topics + память + резерв бюджета", async () => {
    await seedChatEvent("3301", "private");
    const { intentionId, item } = await queueIntention("check_in", "ремонт квартиры", "3301");
    const { model, calls } = countingModel({
      decision: "answer",
      messages: ["ну что, как ремонт?"],
      notes: ["спросил про ремонт"],
      memory: [{ kind: "thought", text: "хочу узнать про ремонт", entities: [], salience: 0.7 }],
    });
    const budget = new BudgetRepo(pool);

    await handlerWith(model, { modelBudget: budget })(item, SIGNAL());

    expect(calls()).toBe(1);
    // инициативный turn — тот же путь доставки, что разговорный ответ
    const turns = await pool.query<{ id: string; status: string }>(
      "SELECT id::text, status FROM turns WHERE chat_id = $1",
      ["3301"],
    );
    expect(turns.rows).toHaveLength(1);
    expect(turns.rows[0]!.status).toBe("applied");
    const messages = await pool.query<{ text: string; position: number; status: string }>(
      "SELECT payload->>'text' AS text, position, status FROM delivery_outbox WHERE turn_id = $1 ORDER BY position",
      [turns.rows[0]!.id],
    );
    expect(messages.rows).toEqual([
      { text: "ну что, как ремонт?", position: 0, status: "pending" },
    ]);
    // журнал инициативной отправки
    const log = await pool.query<{ chat_id: string; intention_id: string }>(
      "SELECT chat_id::text, intention_id::text FROM proactive_log",
    );
    expect(log.rows).toEqual([{ chat_id: "3301", intention_id: intentionId }]);
    // подъём темы зафиксирован (§4.6): topic-entity + подавление инициативы
    const scope = chatScope("3301");
    const topicEntity = await pool.query<{ id: string }>(
      "SELECT id::text FROM entities WHERE kind = 'topic' AND canonical_name = $1 AND scope_id = $2",
      ["ремонт квартиры", scope],
    );
    expect(topicEntity.rows).toHaveLength(1);
    expect(await initiative.isSuppressed(topicEntity.rows[0]!.id, scope)).toBe(true);
    // память кандидатов с источником intention:{id}
    const mem = await pool.query<{ text: string; ref: string }>(
      `SELECT mi.text, s.ref FROM memory_items mi
       JOIN memory_sources ms ON ms.memory_id = mi.id
       JOIN sources s ON s.id = ms.source_id
       WHERE mi.text = 'хочу узнать про ремонт'`,
    );
    expect(mem.rows).toEqual([{ text: "хочу узнать про ремонт", ref: `intention:${intentionId}` }]);
    // заметка хода + завершение
    expect(await scalar("FROM state_notes WHERE work_item_id = $1", [item.id])).toBe(1);
    expect((await intentionRow(intentionId))!.status).toBe("completed");
    expect(await scalar("FROM work_items WHERE id = $1 AND status = 'done'", [item.id])).toBe(1);
    // общий резерв бюджета: детерминированный operation_id (1.9, §16)
    const reservations = await pool.query<{ operation_id: string }>(
      "SELECT operation_id FROM budget_reservations",
    );
    expect(reservations.rows).toEqual([{ operation_id: `model:${item.id}:1` }]);
  });

  it("модель ignore → намерение completed, ничего не отправлено и не записано в журнал", async () => {
    await seedChatEvent("3302", "private");
    const { intentionId, item } = await queueIntention("share", "находка", "3302");
    const { model, calls } = countingModel({ decision: "ignore" });

    await handlerWith(model)(item, SIGNAL());

    expect(calls()).toBe(1);
    expect((await intentionRow(intentionId))!.status).toBe("completed");
    expect(await scalar("FROM turns")).toBe(0);
    expect(await scalar("FROM delivery_outbox")).toBe(0);
    expect(await scalar("FROM proactive_log")).toBe(0);
  });
});

// ---------- exploration §15/§16 ----------

describe("exploration (§15: только память; §16: лимит 2/сутки)", () => {
  it("успех: память thought записана, НИЧЕГО не отправлено, счётчик = 1", async () => {
    await seedChatEvent("3401", "private");
    const { intentionId, item } = await queueIntention("explore", "история парусников", "3401");
    const { model, calls } = countingModel({
      decision: "ignore",
      memory: [{ kind: "thought", text: "кливеры появились в 1800-х", entities: [], salience: 0.6 }],
    });

    await handlerWith(model)(item, SIGNAL());

    expect(calls()).toBe(1);
    expect((await intentionRow(intentionId))!.status).toBe("completed");
    expect(await repo.exploreCompletedToday()).toBe(1);
    expect(await scalar("FROM delivery_outbox")).toBe(0); // §15: никаких отправок
    expect(await scalar("FROM turns")).toBe(0);
    const mem = await pool.query<{ kind: string; text: string }>(
      "SELECT kind, text FROM memory_items WHERE text LIKE '%кливеры%'",
    );
    expect(mem.rows).toEqual([{ kind: "thought", text: "кливеры появились в 1800-х" }]);
    expect(await scalar("FROM work_items WHERE id = $1 AND status = 'done'", [item.id])).toBe(1);
  });

  it("answer при explore → share-намерение ~10 мин, отправки всё равно нет", async () => {
    await seedChatEvent("3402", "private");
    const { intentionId, item } = await queueIntention("explore", "кофе Эфиопии", "3402");
    const { model } = countingModel({ decision: "answer", messages: ["нашёл про кофе"] });

    await handlerWith(model)(item, SIGNAL());

    // отправки нет — находка станет share-намерением (§15)
    expect(await scalar("FROM delivery_outbox")).toBe(0);
    expect(await scalar("FROM turns")).toBe(0);
    expect((await intentionRow(intentionId))!.status).toBe("completed");
    const shares = await pool.query<{ kind: string; topic: string; status: string; not_before: Date }>(
      "SELECT kind, topic, status, not_before FROM intentions WHERE kind = 'share'",
    );
    expect(shares.rows).toHaveLength(1);
    expect(shares.rows[0]!.topic).toBe("кофе Эфиопии");
    expect(shares.rows[0]!.status).toBe("pending");
    const inMinutes = (new Date(shares.rows[0]!.not_before).getTime() - Date.now()) / 60_000;
    expect(inMinutes).toBeGreaterThan(9); // §14.2: исследование → sharing ~10–25 мин
    expect(inMinutes).toBeLessThan(26);
  });

  it("дневной лимит: 2 completed за сегодня → третий explore отменён до модели", async () => {
    await seedChatEvent("3403", "private");
    await pool.query(`
      INSERT INTO intentions (kind, target_chat_id, topic, reason, status, not_before, updated_at, dedupe_key)
      VALUES ('explore', 3403, 'e1', 'r', 'completed', now(), now(), 'fixture:explore:e1:3403'),
             ('explore', 3403, 'e2', 'r', 'completed', now(), now(), 'fixture:explore:e2:3403')`);
    const { intentionId, item } = await queueIntention("explore", "третья тема", "3403");
    const { model, calls } = countingModel({ decision: "ignore" });

    await handlerWith(model)(item, SIGNAL());

    const intention = await intentionRow(intentionId);
    expect(intention!.status).toBe("cancelled");
    expect(intention!.reason).toContain("лимит");
    expect(calls()).toBe(0);
    expect(await repo.exploreCompletedToday()).toBe(2); // лимит не нарушен
  });

  it("счётчик не мешает: вчерашний completed и сегодняшний cancelled не считаются", async () => {
    await seedChatEvent("3404", "private");
    await pool.query(`
      INSERT INTO intentions (kind, target_chat_id, topic, reason, status, not_before, updated_at, dedupe_key)
      VALUES ('explore', 3404, 'вчера', 'r', 'completed', now(), now() - interval '1 day', 'fixture:explore:вчера:3404'),
             ('explore', 3404, 'отменён', 'r', 'cancelled', now(), now(), 'fixture:explore:отменён:3404')`);
    const { item } = await queueIntention("explore", "новая тема", "3404");
    const { model, calls } = countingModel({ decision: "ignore" });

    await handlerWith(model)(item, SIGNAL());

    expect(calls()).toBe(1); // quota свободна — модель звалась
    expect(await repo.exploreCompletedToday()).toBe(1);
  });
});

// ---------- сбой ПОСЛЕ применения: повторное исполнение ----------

/** Outbox-репо, чей enqueue падает один раз — сбой ВНУТРИ транзакции
 * применения (CAS+turn+outbox атомарны: откатываются целиком). */
class FlakyOutboxRepo extends OutboxRepo {
  public failEnqueueOnce = false;

  override async enqueueMessages(
    client: import("pg").PoolClient,
    turnId: string,
    chatId: string,
    texts: string[],
    bubbleDelayMs?: number,
  ): Promise<number> {
    if (this.failEnqueueOnce) {
      this.failEnqueueOnce = false;
      throw new Error("инъекция сбоя: outbox в транзакции применения");
    }
    return super.enqueueMessages(client, turnId, chatId, texts, bubbleDelayMs);
  }
}

describe("атомарность применения намерения: CAS+turn+outbox одной транзакцией (§12.1)", () => {
  it("CAS+turn+outbox атомарны: откат → намерение не завершено → повтор применяет один раз", async () => {
    await seedChatEvent("3501", "private");
    const { intentionId, item } = await queueIntention("check_in", "ремонт", "3501");
    const flaky = new FlakyOutboxRepo(pool);
    flaky.failEnqueueOnce = true;
    const { model, calls } = countingModel({ decision: "answer", messages: ["как ремонт?"] });

    // --- первый прогон: сбой ВНУТРИ применения — всё откатилось ---
    await expect(handlerWith(model, { outboxRepo: flaky })(item, SIGNAL())).rejects.toThrow(
      "инъекция сбоя",
    );
    expect(calls()).toBe(1);
    expect(await scalar("FROM turns")).toBe(0);
    expect(await scalar("FROM delivery_outbox")).toBe(0);
    expect(await scalar("FROM proactive_log")).toBe(0);
    // намерение НЕ завершено (CAS откатился вместе с применением)
    const statusAfterCrash = (await intentionRow(intentionId))!.status;
    expect(statusAfterCrash).toBe("running"); // markRunning закоммичен, CAS откатился

    // --- worker вернул работу; повтор применяет ровно один раз ---
    await workRepo.requeue(item.id);
    const retry = await workRepo.claimNext();
    expect(retry!.id).toBe(item.id);
    await handlerWith(model)(retry!, SIGNAL());

    expect(calls()).toBe(2); // повторная генерация — допустимая потеря (§12.2)
    expect(await scalar("FROM turns")).toBe(1);
    expect(await scalar("FROM delivery_outbox")).toBe(1);
    expect(await scalar("FROM proactive_log")).toBe(1);
    expect((await intentionRow(intentionId))!.status).toBe("completed");
  });
});


// ---------- обязательные доработки критика этапа 5 ----------

/** Репо, пропускающее политику §14.3 (для теста гонки применения). */
class PolicyFreeIntentionRepo extends IntentionRepo {
  override async proactiveAllowed(): Promise<boolean> {
    return true;
  }
}

describe("CAS-перекрытие: победитель один (регрессия Н1, критик этапа 5)", () => {
  it("инициатива: проигравшая параллельная попытка не дублирует отправку", async () => {
    await seedChatEvent("3601", "private");
    const { intentionId, item } = await queueIntention("check_in", "гонка", "3601");
    const result: TurnResult = {
      decision: "answer",
      messages: ["одно сообщение"],
      notes: [],
      memory: [],
      intentions: [],
    };
    const gates: Array<() => void> = [];
    const gatedModel: CharacterModel = {
      async runTurn(): Promise<TurnResult> {
        await new Promise<void>((resolve) => gates.push(resolve));
        return result;
      },
    };
    const policyFree = new PolicyFreeIntentionRepo(pool);
    const handler = handlerWith(gatedModel, { intentionsRepo: policyFree });

    // попытка 1 висит в модели; работа переочередена (timeout-requeue)
    const first = handler(item, SIGNAL());
    await waitFor(() => Promise.resolve(gates.length === 1), { label: "попытка 1 в модели" });
    await workRepo.requeue(item.id);
    const retry = await workRepo.claimNext();
    expect(retry!.id).toBe(item.id);

    // попытка 2 тоже доходит до модели (политика заглушена — гонка доходит до CAS)
    const second = handler(retry!, SIGNAL());
    await waitFor(() => Promise.resolve(gates.length === 2), { label: "попытка 2 в модели" });

    // попытка 1 применяет и завершает
    gates[0]!();
    await first;
    expect((await intentionRow(intentionId))!.status).toBe("completed");

    // попытка 2 доходит до транзакции — CAS отклоняет, дублей нет
    gates[1]!();
    await second;
    expect(await scalar("FROM turns")).toBe(1);
    expect(await scalar("FROM delivery_outbox")).toBe(1);
    expect(await scalar("FROM proactive_log")).toBe(1);
  });

  it("explore: проигравшая попытка не дублирует память", async () => {
    await seedChatEvent("3602", "private");
    const { intentionId, item } = await queueIntention("explore", "теория струн", "3602");
    const result: TurnResult = {
      decision: "ignore",
      messages: [],
      notes: [],
      memory: [
        { kind: "thought", text: "тема глубже, чем кажется", entities: [], salience: 0.6 },
      ],
      intentions: [],
    };
    const gates: Array<() => void> = [];
    const gatedModel: CharacterModel = {
      async runTurn(): Promise<TurnResult> {
        await new Promise<void>((resolve) => gates.push(resolve));
        return result;
      },
    };
    const handler = handlerWith(gatedModel);

    const first = handler(item, SIGNAL());
    await waitFor(() => Promise.resolve(gates.length === 1), { label: "explore 1 в модели" });
    await workRepo.requeue(item.id);
    const retry = await workRepo.claimNext();
    const second = handler(retry!, SIGNAL());
    await waitFor(() => Promise.resolve(gates.length === 2), { label: "explore 2 в модели" });

    gates[0]!();
    await first;
    gates[1]!();
    await second;

    expect((await intentionRow(intentionId))!.status).toBe("completed");
    const thoughts = await pool.query<{ text: string }>(
      "SELECT text FROM memory_items WHERE text LIKE '%глубже%'",
    );
    expect(thoughts.rows).toHaveLength(1);
    expect(await scalar("FROM delivery_outbox")).toBe(0);
  });
});

describe("фоновый промпт содержит настроение и отношение (Н6, §11.1)", () => {
  it("инициатива и explore видят moodLine/relationshipLine в context.system", async () => {
    await seedChatEvent("3603", "private");
    const moodRepo = new MoodRepo(pool);
    await withTransaction(pool, (client) =>
      moodRepo.applyDelta(client, PERSONA_STUB.moodBaseline, { valence: 0.5 }),
    );
    await withTransaction(pool, async (client) => {
      const person = await memory.entities.findOrCreate(client, "person", "Тест", "chat:3603");
      await client.query(
        `INSERT INTO relationships (person_entity_id, scope_id, familiarity, affinity, trust)
         VALUES ($1, 'chat:3603', 0.7, 0.5, 0.8)`,
        [person.id],
      );
    });

    for (const kind of ["check_in", "explore"] as const) {
      const captured: string[] = [];
      const captureModel: CharacterModel = {
        async runTurn(input): Promise<TurnResult> {
          captured.push(input.context.system);
          return { decision: "ignore", messages: [], notes: [], memory: [], intentions: [] };
        },
      };
      const { item } = await queueIntention(kind, "контекст", "3603");
      await handlerWith(captureModel)(item, SIGNAL());
      expect(captured).toHaveLength(1);
      expect(captured[0]).toContain("Твоё состояние сейчас: в приподнятом настроении");
      expect(captured[0]).toContain("Отношение к собеседнику");
    }
  });
});

describe("детерминизм chat_type (Н4): первое событие чата решает", () => {
  it("group-событие раньше private: инициатива отменена как групповая", async () => {
    // два события: сначала группа, потом личка — берётся ПЕРВОЕ (ORDER BY id)
    await pool.query(
      `INSERT INTO social_events (telegram_update_id, chat_id, chat_type, kind, raw_update, status)
       VALUES (900001, 3604, 'group', 'message', '{}', 'journal_only')`,
    );
    await pool.query(
      `INSERT INTO social_events (telegram_update_id, chat_id, chat_type, kind, raw_update, status)
       VALUES (900002, 3604, 'private', 'message', '{}', 'journal_only')`,
    );
    const { intentionId, item } = await queueIntention("check_in", "тип чата", "3604");
    const { model } = countingModel({ decision: "answer", messages: ["привет"] });

    await handlerWith(model)(item, SIGNAL());

    expect((await intentionRow(intentionId))!.status).toBe("cancelled");
    expect(await scalar("FROM turns")).toBe(0);
  });
});
