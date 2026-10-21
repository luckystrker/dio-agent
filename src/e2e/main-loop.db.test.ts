// Сквозной E2E главного цикла §1 архитектуры (этап 6.1): живая БД, настоящие
// модули целиком — ingest → буфер → turn → worker → модель (транспорт-заглушка)
// → применение (память+источник, mood, отношения, намерения) → outbox → доставка.
// Модель детерминирована сценарной заглушкой транспорта, остальное — прод.
// Фазы отмечены по шагам §1: встреча темы (1), сохранение с источником (2),
// самостоятельное изучение (3), возврат к теме через дни (5), пересмотр (6).
// Шаг 4 (stance-писатель) в MVP не подключён — см. бэклог; преемственность
// проверяется через одновременную доступность обеих версий claim (§10.3).

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { ingestUpdate } from "../telegram/ingest.js";
import { ChatBufferManager } from "../telegram/buffers.js";
import { OutboxRepo } from "../telegram/outbox.js";
import { DeliveryLoop } from "../telegram/delivery.js";
import type { TelegramApi, SendMessageResult } from "../telegram/api.js";
import { OpenAICompatModelAdapter, type ModelTransport } from "../character/model-adapter.js";
import { makeTurnHandler } from "../character/turn-handler.js";
import { makeIntentionHandler } from "../background/intention-handler.js";
import { IntentionRepo } from "../background/intentions.js";
import { InitiativeRepo } from "../character/initiative.js";
import { PersonaRepo } from "../character/persona.js";
import { MoodRepo } from "../character/mood.js";
import { RelationshipRepo } from "../character/relationships.js";
import { MemoryRepo } from "../memory/memory-repo.js";
import { BudgetRepo } from "../runtime/budgets.js";
import { WorkQueueRepo } from "../runtime/work-queue.js";
import { CognitiveWorker } from "../runtime/worker.js";
import { createTestPool, cleanMemoryTables } from "../test-support/db.js";
import { cleanTelegramTables, silentLogger, waitForDb } from "../test-support/telegram-db.js";

const CHAT_ID = "300111";
const ACTOR_ID = 4242;

/** Ответ модели: tool-вызов turn_result + usage для журнала хода. */
function modelResponse(turnResult: Record<string, unknown>): Response {
  return new Response(
    JSON.stringify({
      choices: [
        {
          message: {
            tool_calls: [
              {
                type: "function",
                function: { name: "turn_result", arguments: JSON.stringify(turnResult) },
              },
            ],
          },
        },
      ],
      usage: { prompt_tokens: 120, completion_tokens: 25 },
    }),
  );
}

interface CapturedRequest {
  system: string;
  messages: Array<{ role: string; content: string }>;
}

/** Транспорт-заглушка: выдаёт сценарные ответы по очереди, пишет запросы. */
function makeScriptedModel(script: Array<Record<string, unknown>>, captured: CapturedRequest[]) {
  let next = 0;
  const transport: ModelTransport = async (_url, init) => {
    const body = JSON.parse(init.body) as { messages: CapturedRequest["messages"] };
    const system = body.messages.find((message) => message.role === "system");
    captured.push({
      system: system?.content ?? "",
      messages: body.messages.filter((message) => message.role !== "system"),
    });
    const result = script[next];
    next += 1;
    if (!result) throw new Error("сценарий модели исчерпан: непредвиденный вызов LLM");
    return modelResponse(result);
  };
  return new OpenAICompatModelAdapter({ endpoint: "http://stub", apiKey: "k", model: "stub", transport });
}

function rawUpdate(updateId: number, text: string): unknown {
  return {
    update_id: updateId,
    message: {
      message_id: updateId,
      from: { id: ACTOR_ID, is_bot: false },
      chat: { id: Number(CHAT_ID), type: "private" },
      date: Math.floor(Date.now() / 1000),
      text,
    },
  };
}

/** Telegram API в памяти: доставку видим, ничего вовне не уходит. */
function makeRecordingApi(sent: Array<{ chatId: string; text: string }>): TelegramApi {
  return {
    getUpdates: () => new Promise(() => undefined), // poller в E2E не участвует
    sendMessage: async (chatId, text) => {
      sent.push({ chatId, text });
      const result: SendMessageResult = { messageId: String(sent.length) };
      return result;
    },
    setMessageReaction: async () => undefined,
  };
}

const CLAIM_DAY0 =
  "Источник S утверждает: число аварий в новогодние праздники растёт год от года";
const CLAIM_REVISED = "Новая официальная статистика: аварий в эти праздники стало меньше";

describe("E2E: главный цикл §1 — тема → память с источником → изучение → возврат → пересмотр", () => {
  let pool: Pool;
  let nextUpdateId = 90_000;
  let captured: CapturedRequest[];
  let sent: Array<{ chatId: string; text: string }>;

  beforeEach(async () => {
    pool = createTestPool();
    captured = [];
    sent = [];
    await cleanTelegramTables(pool);
    await cleanMemoryTables(pool);
    await pool.query("DELETE FROM mood_state");
    await pool.query("TRUNCATE TABLE proactive_log, intentions RESTART IDENTITY CASCADE");
    await pool.query("TRUNCATE TABLE personas RESTART IDENTITY CASCADE");
    nextUpdateId = 90_000;
  });

  afterEach(async () => {
    await pool.end();
  });

  /** Прогнать сообщения через ingest → буфер (шов poller→buffer). */
  async function feed(texts: string[], buffers: ChatBufferManager): Promise<void> {
    for (const text of texts) {
      const result = await ingestUpdate(pool, rawUpdate(nextUpdateId++, text));
      expect(result.inserted).toBe(true);
      buffers.add({
        eventId: result.eventId!,
        chatId: CHAT_ID,
        chatType: "private",
        kind: "message",
        text,
        occurredAt: new Date(),
      });
    }
  }

  async function turnRow(where: { chatId?: string; status?: string }) {
    const conditions: string[] = [];
    const params: unknown[] = [];
    if (where.chatId !== undefined) {
      params.push(where.chatId);
      conditions.push(`chat_id = $${params.length}`);
    }
    if (where.status !== undefined) {
      params.push(where.status);
      conditions.push(`status = $${params.length}`);
    }
    const result = await pool.query(
      `SELECT id::text, chat_id, status FROM turns WHERE ${conditions.join(" AND ")} ORDER BY id DESC LIMIT 1`,
      params,
    );
    return result.rows[0] as { id: string; chat_id: string; status: string } | undefined;
  }

  it(
    "полный цикл: разговор сохраняет claim с источником, exploration изучает тему, через дни retrieval возвращает основания, противоречие добавляет новую версию",
    { timeout: 120_000 },
    async () => {
      // --- Сборка процесса как в index.ts ---
      const personaRepo = new PersonaRepo(pool);
      const persona = await personaRepo.active();
      const memory = new MemoryRepo(pool);
      const mood = new MoodRepo(pool);
      const relationships = new RelationshipRepo(pool);
      const modelBudget = new BudgetRepo(pool);
      const intentionsRepo = new IntentionRepo(pool);
      const initiativeRepo = new InitiativeRepo(pool);
      const outbox = new OutboxRepo(pool);
      const workRepo = new WorkQueueRepo(pool);

      const script: Array<Record<string, unknown>> = [
        // ход 1 (день 0): значимая тема → ответ + claim/thought + explore-намерение
        {
          decision: "answer",
          messages: ["Мда, мрачная статистика.", "Давай лучше дома посидим в этот год."],
          notes: ["тема: безопасность дорог в праздники"],
          memory: [
            {
              kind: "claim",
              text: CLAIM_DAY0,
              entities: [
                { name: "новый год", kind: "event", role: "context" },
                { name: "дорожная безопасность", kind: "topic", role: "subject" },
              ],
              salience: 0.8,
              confidence: 0.6,
            },
            {
              kind: "thought",
              text: "Праздничная суета вызывает неприятные ассоциации из-за историй про аварии",
              entities: [{ name: "новый год", kind: "event", role: "subject" }],
              salience: 0.6,
            },
          ],
          intentions: [
            {
              kind: "explore",
              topic: "безопасность праздничных поездок",
              reason: "хочу разобраться в статистике аварий",
              delay_hours: 24,
            },
          ],
          mood_delta: { valence: -0.2, reason: "мрачная тема аварий" },
          relationship_delta: { affinity: 0.1, reason: "собеседник делится важным" },
        },
        // ход 2 (фон, +24 ч): exploration — только память, без отправки (§15)
        {
          decision: "ignore",
          notes: ["изучил тему самостоятельно"],
          memory: [
            {
              kind: "experience",
              text: "Самостоятельно разобрался в статистике аварий праздничных поездок",
              entities: [{ name: "дорожная безопасность", kind: "topic", role: "subject" }],
              salience: 0.5,
            },
          ],
        },
        // ход 3 (через дни): вспоминание — ответ с опорой на память
        {
          decision: "answer",
          messages: ["Помню. Источник тогда писал, что аварий становится больше — я это запомнила."],
          notes: ["вспомнила тему по прямому вопросу"],
        },
        // ход 4: противоречащий опыт → новая версия claim
        {
          decision: "answer",
          messages: ["О, это меняет дело — значит, моя прежняя картинка устарела."],
          notes: ["пересмотр отношения к теме"],
          memory: [
            {
              kind: "claim",
              text: CLAIM_REVISED,
              entities: [{ name: "дорожная безопасность", kind: "topic", role: "subject" }],
              salience: 0.8,
              confidence: 0.7,
            },
          ],
        },
      ];
      const model = makeScriptedModel(script, captured);

      const handleTurn = makeTurnHandler({
        pool,
        model,
        outbox,
        memory,
        intentions: intentionsRepo,
        persona,
        mood,
        relationships,
        workQueue: workRepo,
        modelBudget,
        modelDailyCallsLimit: 50,
        logger: silentLogger,
      });
      const handleIntention = makeIntentionHandler({
        pool,
        model,
        mood,
        memory,
        persona,
        outbox,
        intentions: intentionsRepo,
        initiative: initiativeRepo,
        workQueue: workRepo,
        modelBudget,
        modelDailyCallsLimit: 50,
        logger: silentLogger,
      });

      const buffers = new ChatBufferManager({ pool, logger: silentLogger });
      const api = makeRecordingApi(sent);
      const delivery = new DeliveryLoop({ pool, repo: outbox, api, logger: silentLogger }, { intervalMs: 100 });
      const worker = new CognitiveWorker(
        workRepo,
        { direct: handleTurn, group: handleTurn, intention: handleIntention },
        silentLogger,
        { pollIntervalMs: 50 },
      );
      worker.start();
      delivery.start();

      // ---------- §1 шаг 1–2: значимая тема сохраняется с источником ----------
      await feed(
        ["Привет! Читал данные, что аварий в новогодние праздники становится больше — правда жуть."],
        buffers,
      );

      await waitForDb(async () => {
        const turn = await turnRow({ chatId: CHAT_ID, status: "applied" });
        return turn !== undefined;
      }, "ход дня 0 применён");

      // события хода обработаны, ответ доставлен (два пузыря по порядку)
      await waitForDb(async () => {
        const events = await pool.query<{ status: string }>(
          "SELECT status FROM social_events WHERE chat_id = $1",
          [CHAT_ID],
        );
        return events.rows.length === 1 && events.rows[0]!.status === "processed";
      }, "событие processed");
      await waitForDb(async () => {
        const rows = await pool.query<{ status: string; position: number }>(
          "SELECT status, position FROM delivery_outbox WHERE chat_id = $1 ORDER BY position",
          [CHAT_ID],
        );
        return rows.rows.length === 2 && rows.rows.every((row) => row.status === "sent");
      }, "оба сообщения доставлены");
      expect(sent.map((message) => message.text)).toEqual([
        "Мда, мрачная статистика.",
        "Давай лучше дома посидим в этот год.",
      ]);
      expect(sent[0]!.chatId).toBe(CHAT_ID);

      // claim и thought записаны и активны; claim имеет источник journal_event → turn
      const turn0 = (await turnRow({ chatId: CHAT_ID, status: "applied" }))!;
      const items = await pool.query<{ id: string; kind: string; text: string; status: string }>(
        "SELECT id::text, kind, text, status FROM memory_items WHERE scope_id = $1 ORDER BY id",
        [`chat:${CHAT_ID}`],
      );
      expect(items.rows.map((row) => row.kind).sort()).toEqual(["claim", "thought"]);
      const claim = items.rows.find((row) => row.kind === "claim")!;
      expect(claim.text).toBe(CLAIM_DAY0);
      expect(claim.status).toBe("active");
      const source = await pool.query<{ kind: string; ref: string }>(
        `SELECT s.kind, s.ref FROM memory_sources ms
         JOIN sources s ON s.id = ms.source_id WHERE ms.memory_id = $1`,
        [claim.id],
      );
      expect(source.rows).toEqual([{ kind: "journal_event", ref: `turn:${turn0.id}` }]);

      // настроение и отношения сдвинулись от хода (§7.2–7.3)
      const moodRow = await pool.query<{ valence: string }>("SELECT valence FROM mood_state WHERE id = 1");
      expect(Number(moodRow.rows[0]!.valence)).toBeCloseTo(0.1 - 0.2, 5);
      const relRow = await pool.query<{ familiarity: string; affinity: string }>(
        "SELECT familiarity, affinity FROM relationships WHERE scope_id = $1",
        [`chat:${CHAT_ID}`],
      );
      expect(Number(relRow.rows[0]!.familiarity)).toBeGreaterThan(0);
      expect(Number(relRow.rows[0]!.affinity)).toBeGreaterThan(0);

      // модельный вызов оплачен из дневного бюджета (§16)
      const budgetRows = await pool.query<{ operation_id: string }>(
        "SELECT operation_id FROM budget_reservations WHERE operation_id LIKE 'model:%'",
      );
      expect(budgetRows.rows.length).toBeGreaterThanOrEqual(1);

      // explore-намерение создано и ждёт суток (§14.1)
      const intention = await pool.query<{ id: string; kind: string; status: string; not_before: Date }>(
        "SELECT id::text, kind, status, not_before FROM intentions WHERE kind = 'explore'",
      );
      expect(intention.rows).toHaveLength(1);
      expect(intention.rows[0]!.status).toBe("pending");
      expect(intention.rows[0]!.not_before.getTime()).toBeGreaterThan(Date.now() + 23 * 3_600_000);
      const intentionId = intention.rows[0]!.id;

      // ---------- §1 шаг 3: самостоятельное изучение (фон, сутки спустя) ----------
      await pool.query("UPDATE intentions SET not_before = now() - interval '1 second' WHERE id = $1", [
        intentionId,
      ]);
      const claimed = await intentionsRepo.claimDue();
      expect(claimed).toBe(1);

      await waitForDb(async () => {
        const row = await pool.query<{ status: string }>(
          "SELECT status FROM intentions WHERE id = $1",
          [intentionId],
        );
        return row.rows[0]!.status === "completed";
      }, "exploration завершён");

      // находка — в памяти с источником runtime_action, ничего не отправлено (§15)
      const experience = await pool.query<{ id: string }>(
        `SELECT m.id::text FROM memory_items m
         JOIN memory_sources ms ON ms.memory_id = m.id
         JOIN sources s ON s.id = ms.source_id
         WHERE s.kind = 'runtime_action' AND s.ref = $1 AND m.kind = 'experience'`,
        [`intention:${intentionId}`],
      );
      expect(experience.rows).toHaveLength(1);
      const outboxCount = await pool.query<{ count: string }>(
        "SELECT count(*)::text AS count FROM delivery_outbox",
      );
      expect(outboxCount.rows[0]!.count).toBe("2");

      // ---------- §1 шаг 5: возврат к теме через дни — основания в контексте ----------
      await feed(["Помнишь, ты разбиралась с авариями в праздники? Что там со статистикой?"], buffers);

      await waitForDb(async () => {
        const turns = await pool.query("SELECT id FROM turns WHERE chat_id = $1", [CHAT_ID]);
        return turns.rows.length === 2;
      }, "второй turn создан");
      await waitForDb(async () => {
        const turn = await turnRow({ chatId: CHAT_ID });
        return turn?.status === "applied";
      }, "второй ход применён");

      // контекст хода собран из БД: день-0 claim всплыл через retrieval,
      // в истории — фактически отправленные ответы персонажа
      const recallRequest = captured.at(-1)!;
      expect(recallRequest.system).toContain(CLAIM_DAY0);
      expect(recallRequest.messages).toEqual(
        expect.arrayContaining([
          { role: "assistant", content: "Мда, мрачная статистика." },
          { role: "assistant", content: "Давай лучше дома посидим в этот год." },
        ]),
      );
      await waitForDb(async () => {
        const rows = await pool.query<{ status: string }>(
          "SELECT status FROM delivery_outbox WHERE chat_id = $1",
          [CHAT_ID],
        );
        return rows.rows.length === 3 && rows.rows.every((row) => row.status === "sent");
      }, "ответ воспоминания доставлен");

      // ---------- §1 шаг 6: противоречащий опыт → новая версия рядом со старой ----------
      await feed(["Разобрался: официальная статистика говорит — аварий в эти праздники стало меньше."], buffers);

      await waitForDb(async () => {
        const turns = await pool.query("SELECT id FROM turns WHERE chat_id = $1", [CHAT_ID]);
        return turns.rows.length === 3;
      }, "третий turn создан");
      await waitForDb(async () => {
        const turn = await turnRow({ chatId: CHAT_ID });
        return turn?.status === "applied";
      }, "третий ход применён");

      const claims = await pool.query<{ text: string; status: string }>(
        "SELECT text, status FROM memory_items WHERE kind = 'claim' AND scope_id = $1 ORDER BY id",
        [`chat:${CHAT_ID}`],
      );
      expect(claims.rows.map((row) => row.text)).toEqual([CLAIM_DAY0, CLAIM_REVISED]);
      // обе версии активны и доступны retrieval — преемственность не потеряна (§10.3);
      // автоматический supersede ходом не выполняется — писатель стances в бэклоге
      expect(claims.rows.every((row) => row.status === "active")).toBe(true);
      const recall = await memory.retrieve(`chat:${CHAT_ID}`, {
        textQuery: "аварии праздники",
        limit: 10,
      });
      expect(recall.map((row) => row.text)).toEqual(expect.arrayContaining([CLAIM_DAY0, CLAIM_REVISED]));

      // ---------- очистка процессов ----------
      await worker.stop();
      await delivery.stop();
    },
  );

  it("повторная пачка тех же update'ов не создаёт ни событий, ни хода (идемпотентность ingest)", async () => {
    const buffers = new ChatBufferManager({ pool, logger: silentLogger });
    const update = rawUpdate(nextUpdateId++, "дубль пачки");
    const first = await ingestUpdate(pool, update);
    expect(first.inserted).toBe(true);
    buffers.add({
      eventId: first.eventId!,
      chatId: CHAT_ID,
      chatType: "private",
      kind: "message",
      text: "дубль пачки",
      occurredAt: new Date(),
    });

    const second = await ingestUpdate(pool, update);
    expect(second.inserted).toBe(false);
    expect(second.eventId).toBeNull();

    const events = await pool.query("SELECT id FROM social_events");
    expect(events.rows).toHaveLength(1);
    const turns = await pool.query("SELECT id FROM turns");
    expect(turns.rows).toHaveLength(0); // дубль не попал в буфер — ход не создан
  });
});
