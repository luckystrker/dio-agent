import type { Pool } from "pg";
import type { Logger } from "../runtime/logger.js";
import type { WorkHandler } from "../runtime/worker.js";
import type { WorkQueueRepo } from "../runtime/work-queue.js";
import { SkipWorkError } from "../runtime/worker.js";
import { withTransaction } from "../runtime/transactions.js";
import { applyDeltas, validateDeltas } from "../runtime/deltas.js";
import { reserveModelCall } from "../runtime/model-budget.js";
import type { Budget } from "../runtime/budgets.js";
import type { CharacterModel } from "../character/model-adapter.js";
import type { Persona } from "../character/persona.js";
import { buildSystemPrompt } from "../character/persona.js";
import { describeMood, type MoodRepo } from "../character/mood.js";
import { describeRelationship } from "../character/relationships.js";
import { MemoryRepo } from "../memory/memory-repo.js";
import { validateMemoryCandidates } from "../memory/memory-candidates.js";
import { chatScope } from "../memory/scope.js";
import { InitiativeRepo } from "../character/initiative.js";
import type { OutboxRepo } from "../telegram/outbox.js";
import { IntentionRepo } from "./intentions.js";
import type { IntentionRow } from "./intentions.js";

// Исполнение намерения (§14–§15): свежий контекст-гейт, проактивная
// политика §14.3, откладывание при прямом обращении (§5.2), exploration
// без отправки (§15) с лимитом 2/сутки (§16).

/** §16: до 2 exploration в сутки. */
export const MAX_EXPLORATIONS_PER_DAY = 2;

export interface IntentionHandlerDeps {
  pool: Pool;
  model: CharacterModel | null;
  mood: MoodRepo;
  memory: MemoryRepo;
  persona: Persona;
  outbox: OutboxRepo;
  intentions: IntentionRepo;
  initiative: InitiativeRepo;
  workQueue: WorkQueueRepo;
  modelBudget: Budget | null;
  modelDailyCallsLimit: number;
  /** Каркас системного промпта (config/system-prompt.md, §7.1); пусто — встроенный по умолчанию */
  promptTemplate?: string;
  logger: Logger;
}

const INITIATIVE_SYSTEM_ADDENDUM =
  "Ты сам решил написать первым (инициатива). Будь уместен и кратко: одна мысль, без дежурных извинений. " +
  "Если за это время собеседник уже писал тебе на другую тему — лучше ignore. " +
  "Новых намерений (intentions) в этом ходе не предлагай — инициатива уже случилась.";

const EXPLORE_SYSTEM_ADDENDUM =
  "Ты изучаешь тему для себя (exploration). НИЧЕГО не отправляй собеседнику: " +
  "decision=ignore, а находки оформи в memory (thought/preference/claim о теме).";

export function makeIntentionHandler(deps: IntentionHandlerDeps): WorkHandler {
  const { pool, memory, persona, outbox, intentions, initiative, workQueue, logger } = deps;
  const theModel = deps.model; // null отсеян в начале хода; алиас для замыканий

  return async function handleIntention(item, signal) {
    if (!theModel) {
      throw new SkipWorkError("модель не настроена", "модель не настроена (MODEL_ENDPOINT/API_KEY/MODEL)");
    }
    const activeModel: CharacterModel = theModel;
    const payload = (item.payload ?? {}) as { intentionId?: string };
    const intentionId = typeof payload.intentionId === "string" ? payload.intentionId : null;
    if (!intentionId) {
      throw new SkipWorkError("работа без intentionId", "работа intention без intentionId");
    }

    const intention = await intentions.byId(intentionId);
    // §12.2: queued и running выполнимы (running — повтор после сбоя/ошибки
    // попытки); терминальные статусы — работа закрывается без действий
    if (!intention || (intention.status !== "queued" && intention.status !== "running")) {
      logger.debug("намерение в терминальном статусе — работа закрывается", {
        intentionId,
        status: intention?.status,
      });
      await workQueue.complete(item.id);
      return;
    }

    // §5.2: при прямом обращении фоновый шаг откладывается
    if (await directWorkPending()) {
      await workQueue.requeue(item.id, 5_000);
      logger.info("намерение отложено: в очереди прямое обращение (§5.2)", { intentionId });
      return;
    }

    // Просрочено — честная отмена (§14.1 expiresAt)
    if (intention.expiresAt !== null && intention.expiresAt.getTime() < Date.now()) {
      await intentions.cancel(intentionId, "срок истёк (expiresAt)");
      await workQueue.complete(item.id);
      logger.info("намерение отменено: истёк срок", { intentionId });
      return;
    }

    // Свежий контекст-гейт (§14.3): не лезем в чат, где собеседник активен.
    // Раньше сравнивали с createdAt намерения — тогда любое сообщение после
    // создания (даже за сутки до срока) убивало инициативу, и Дио не писал
    // первым никогда. Теперь: если в чате тихо 30 минут — инициатива уместна.
    // explore — внутреннее действие, чат не трогает: гейт ему не нужен.
    if (intention.kind !== "explore" && (await chatIsActive(intention.targetChatId))) {
      await intentions.cancel(intentionId, "чат активен — инициатива неуместна");
      await workQueue.complete(item.id);
      logger.info("намерение отменено: свежий контекст-гейт (§14.3)", { intentionId });
      return;
    }

    await intentions.markRunning(intentionId);

    if (intention.kind === "explore") {
      await runExploration(activeModel, item, intentionId, intention, signal);
      return;
    }
    await runInitiative(activeModel, item, intentionId, intention, signal);
  };

  async function directWorkPending(): Promise<boolean> {
    const result = await pool.query<{ exists: boolean }>(
      `SELECT EXISTS (
         SELECT 1 FROM work_items
         WHERE kind IN ('direct', 'group') AND status = 'pending'
       ) AS exists`,
    );
    return result.rows[0]?.["exists"] === true;
  }

  async function chatIsActive(chatId: string): Promise<boolean> {
    const result = await pool.query<{ exists: boolean }>(
      `SELECT EXISTS (
         SELECT 1 FROM social_events
         WHERE chat_id = $1 AND occurred_at > now() - interval '30 minutes'
       ) AS exists`,
      [chatId],
    );
    return result.rows[0]?.["exists"] === true;
  }

  /** §15: исследование — только память, никаких отправок. */
  async function runExploration(
    activeModel: CharacterModel,
    item: { id: string; attempts: number },
    intentionId: string,
    intention: IntentionRow,
    signal: AbortSignal,
  ): Promise<void> {
    const doneToday = await intentions.exploreCompletedToday();
    if (doneToday >= MAX_EXPLORATIONS_PER_DAY) {
      await intentions.cancel(intentionId, `дневной лимит exploration (${MAX_EXPLORATIONS_PER_DAY}/сутки, §16)`);
      await workQueue.complete(item.id);
      logger.info("exploration отменён: дневной лимит", { intentionId, doneToday });
      return;
    }

    await reserveModelCall(deps.modelBudget, deps.modelDailyCallsLimit, item.id, item.attempts);

    const scopeId = chatScope(intention.targetChatId);
    const related = await memory.retrieve(scopeId, { textQuery: intention.topic, limit: 6 });
    const memoryBlock = related.map((row) => `- (${row.kind}) ${row.text}`).join("\n");
    const extras = await backgroundExtras(scopeId, memoryBlock);

    const result = await activeModel.runTurn({
      turnId: `intention:${intentionId}`,
      sessionId: scopeId,
      context: {
        system: buildSystemPrompt(persona, extras, deps.promptTemplate) + `\n\n${EXPLORE_SYSTEM_ADDENDUM}`,
        messages: [{ role: "user", content: `Тема для размышления: ${intention.topic}. Причина: ${intention.reason}` }],
      },
      signal,
    });
    const candidates = validateMemoryCandidates(result.memory);

    // Применение + завершение ОДНОЙ транзакцией (§12.1, инвариант 4):
    // CAS на статус — победитель один; дублей памяти нет, зомби-running нет
    // (находки Н1–Н3 приёмки этапа 5)
    await withTransaction(pool, async (client) => {
      const won = await client.query(
        `UPDATE intentions SET status = 'completed', updated_at = now()
         WHERE id = $1 AND status IN ('queued', 'running') RETURNING id`,
        [intentionId],
      );
      if ((won.rowCount ?? 0) === 0) {
        logger.warn("exploration уже исполнен параллельной попыткой — пропуск", { intentionId });
        await workQueue.completeWithClient(client, item.id);
        return;
      }

      const source = { kind: "runtime_action" as const, ref: `intention:${intentionId}` };
      for (const candidate of candidates) {
        await memory.insertCandidate(client, candidate, scopeId, source);
      }
      // §15: находка становится share-намерением, а не отправкой
      if (result.decision === "answer" && result.messages.length > 0) {
        await deps.intentions.create(client, {
          kind: "share",
          topic: intention.topic,
          reason: `находка исследования: ${result.reason ?? intention.topic}`,
          targetChatId: intention.targetChatId,
          delayHours: 10 / 60, // §14.2: исследование → sharing ~10–25 мин
        });
      }
      const noteDeltas = validateDeltas(
        result.notes.map((text) => ({ type: "append_note" as const, text })),
      );
      await applyDeltas(client, item.id, noteDeltas);
      await workQueue.completeWithClient(client, item.id);
    });
    logger.info("exploration завершён", { intentionId, topic: intention.topic, findings: candidates.length });
  }

  /** §14.3: инициатива (reply_later/check_in/share) — политика, затем отправка. */
  async function runInitiative(
    activeModel: CharacterModel,
    item: { id: string; attempts: number },
    intentionId: string,
    intention: IntentionRow,
    signal: AbortSignal,
  ): Promise<void> {
    const chatType = await pool.query<{ chat_type: string }>(
      "SELECT chat_type FROM social_events WHERE chat_id = $1 ORDER BY id LIMIT 1",
      [intention.targetChatId],
    );
    const isPrivate = chatType.rows[0]?.["chat_type"] === "private";

    if (!(await intentions.proactiveAllowed(intention.targetChatId, isPrivate))) {
      await intentions.cancel(intentionId, "проактивная политика §14.3 (лимит/пауза/группа)");
      await workQueue.complete(item.id);
      logger.info("инициатива отменена политикой §14.3", { intentionId });
      return;
    }

    await reserveModelCall(deps.modelBudget, deps.modelDailyCallsLimit, item.id, item.attempts);

    const scopeId = chatScope(intention.targetChatId);
    const related = await memory.retrieve(scopeId, { textQuery: intention.topic, limit: 6 });
    const memoryBlock = related.map((row) => `- (${row.kind}) ${row.text}`).join("\n");
    const extras = await backgroundExtras(scopeId, memoryBlock);

    const result = await activeModel.runTurn({
      turnId: `intention:${intentionId}`,
      sessionId: scopeId,
      context: {
        system: buildSystemPrompt(persona, extras, deps.promptTemplate) + `\n\n${INITIATIVE_SYSTEM_ADDENDUM}`,
        messages: [
          {
            role: "user",
            content: `Твоё отложенное намерение: ${intention.kind} по теме «${intention.topic}» (причина: ${intention.reason}). Напиши собеседнику, если уместно.`,
          },
        ],
      },
      signal,
    });

    if (result.decision !== "answer" || result.messages.length === 0) {
      // ignore и отменённое не требуют объяснений пользователю (§14.3)
      await intentions.complete(intentionId);
      await workQueue.complete(item.id);
      logger.info("инициатива: модель решила промолчать", { intentionId, decision: result.decision });
      return;
    }

    const candidates = validateMemoryCandidates(result.memory);
    await withTransaction(pool, async (client) => {
      // CAS-однократность исполнения (§12.1, инвариант 4): победитель один;
      // проигравший повтор (timeout-requeue против брошенного handler) не
      // создаёт второй turn с дублем сообщений и не обходит §14.3 (Н1)
      const won = await client.query(
        `UPDATE intentions SET status = 'completed', updated_at = now()
         WHERE id = $1 AND status IN ('queued', 'running') RETURNING id`,
        [intentionId],
      );
      if ((won.rowCount ?? 0) === 0) {
        logger.warn("инициатива уже исполнена параллельной попыткой — пропуск", { intentionId });
        await workQueue.completeWithClient(client, item.id);
        return;
      }

      // инициативный turn: тот же путь доставки, что и разговорный ответ
      const turn = await client.query<{ id: string }>(
        "INSERT INTO turns (chat_id) VALUES ($1) RETURNING id",
        [intention.targetChatId],
      );
      const turnId = String(turn.rows[0]!.id);
      await client.query("UPDATE turns SET status = 'applied' WHERE id = $1", [turnId]);
      await outbox.enqueueMessages(client, turnId, intention.targetChatId, result.messages);

      await intentions.logProactive(client, intention.targetChatId, intentionId);
      const source = { kind: "runtime_action" as const, ref: `intention:${intentionId}` };
      for (const candidate of candidates) {
        await memory.insertCandidate(client, candidate, scopeId, source);
      }
      const noteDeltas = validateDeltas(
        result.notes.map((text) => ({ type: "append_note" as const, text })),
      );
      await applyDeltas(client, item.id, noteDeltas);
      await workQueue.completeWithClient(client, item.id);
    });
    // §4.6/§14.3: фиксируем подъём темы — повторная инициатива подавляется
    if (result.decision === "answer") {
      await recordTopicInitiative(intention, scopeId);
    }
    logger.info("инициатива отправлена", {
      intentionId,
      kind: intention.kind,
      topic: intention.topic,
      messages: result.messages.length,
    });
  }

  /** §11.1: фоновый ход тоже видит настроение и отношение к собеседнику чата. */
  async function backgroundExtras(
    scopeId: string,
    memoryBlock: string,
  ): Promise<{ moodLine?: string; relationshipLine?: string; memoryBlock?: string }> {
    const extras: { moodLine?: string; relationshipLine?: string; memoryBlock?: string } = {};
    try {
      const moodNow = await deps.mood.readDecayed(persona.moodBaseline);
      extras.moodLine = describeMood(moodNow.mood, persona.moodBaseline);
    } catch {
      // без настроения фоновый ход не срываем
    }
    try {
      const relationship = await pool.query(
        `SELECT familiarity, affinity, trust FROM relationships
         WHERE scope_id = $1 ORDER BY updated_at DESC LIMIT 1`,
        [scopeId],
      );
      const row = relationship.rows[0] as
        | { familiarity: string; affinity: string; trust: string }
        | undefined;
      if (row) {
        extras.relationshipLine = describeRelationship({
          personEntityId: "0",
          scopeId,
          familiarity: Number(row.familiarity),
          affinity: Number(row.affinity),
          trust: Number(row.trust),
          updatedAt: new Date().toISOString(),
        });
      }
    } catch {
      // без отношений фоновый ход не срываем
    }
    if (memoryBlock.length > 0) extras.memoryBlock = memoryBlock;
    return extras;
  }

  async function recordTopicInitiative(intention: IntentionRow, scopeId: string): Promise<void> {
    try {
      const entity = await withTransaction(pool, (client) =>
        memory.entities.findOrCreate(client, "topic", intention.topic, scopeId),
      );
      await initiative.record(entity.id, scopeId);
    } catch (error) {
      logger.warn("не удалось зафиксировать инициативу по теме", {
        intentionId: intention.id,
        error: error instanceof Error ? error.message : String(error),
      });
    }
  }
}
