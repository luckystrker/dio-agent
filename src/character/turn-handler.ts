import type { Pool } from "pg";
import type { Logger } from "../runtime/logger.js";
import type { WorkHandler } from "../runtime/worker.js";
import { SkipWorkError } from "../runtime/worker.js";
import { withTransaction } from "../runtime/transactions.js";
import { applyDeltas, validateDeltas } from "../runtime/deltas.js";
import type { OutboxRepo } from "../telegram/outbox.js";
import type { CharacterModel, ModelTurnContext } from "./model-adapter.js";
import { TurnResultSchema, type TurnResult } from "./turn-result.js";
import { validateIntentionProposals } from "../background/intention-proposals.js";
import type { IntentionRepo } from "../background/intentions.js";
import { validateMemoryCandidates } from "../memory/memory-candidates.js";
import type { MemoryRepo } from "../memory/memory-repo.js";
import { chatScope } from "../memory/scope.js";
import { buildSystemPrompt, type Persona } from "./persona.js";
import { clampMoodDelta, describeMood, MoodRepo, type Mood } from "./mood.js";
import {
  describeRelationship,
  RELATIONSHIP_LIMITS,
  RelationshipRepo,
} from "./relationships.js";
import type { Budget } from "../runtime/budgets.js";
import { reserveModelCall } from "../runtime/model-budget.js";
import type { WorkQueueRepo } from "../runtime/work-queue.js";

// Ход по turn (§12.1–12.2): события → сборка контекста из БД → модель →
// сохранение result_ready → короткая транзакция применения (CAS, outbox,
// дельты состояния, память, закрытие работы). Результат хода переживает
// процесс: после сбоя применяется сохранённый без нового вызова LLM.

export interface TurnHandlerDeps {
  pool: Pool;
  model: CharacterModel | null;
  outbox: OutboxRepo;
  memory: MemoryRepo;
  persona: Persona;
  mood: MoodRepo;
  relationships: RelationshipRepo;
  intentions: IntentionRepo;
  workQueue: WorkQueueRepo;
  /** Дневной бюджет модельных вызовов (§16, 1.9); null — не ограничивать */
  modelBudget: Budget | null;
  modelDailyCallsLimit: number;
  /** Каркас системного промпта (config/system-prompt.md, §7.1); пусто — встроенный по умолчанию */
  promptTemplate?: string;
  logger: Logger;
}

interface TurnActor {
  entityId: string | null;
  name: string | null;
}

export function makeTurnHandler(deps: TurnHandlerDeps): WorkHandler {
  const { pool, model, outbox, memory, persona, mood, relationships, workQueue, logger } = deps;

  return async function handleTurn(item, signal) {
    const startedAtMs = Date.now();
    if (!model) {
      throw new SkipWorkError(
        "модель не настроена",
        "модель не настроена (MODEL_ENDPOINT/API_KEY/MODEL)",
      );
    }
    const payload = (item.payload ?? {}) as { turnId?: string; chatId?: string };
    const turnId = typeof payload.turnId === "string" ? payload.turnId : null;
    const scopeId = typeof payload.chatId === "string" ? chatScope(payload.chatId) : "internal";

    // --- Фаза 0: состояние turn (§12.2): result_ready применяется без LLM ---
    let result: TurnResult;
    let candidates = [] as ReturnType<typeof validateMemoryCandidates>;
    let proposals = [] as ReturnType<typeof validateIntentionProposals>;
    // §20: журнал хода — usage привязывается к этому вызову через onUsage
    let usage: { promptTokens: number; completionTokens: number } | undefined;
    let budgetOperationId: string | null = null;

    const turnRow = turnId
      ? await pool.query<{ status: string; validated_result: unknown }>(
          "SELECT status, validated_result FROM turns WHERE id = $1",
          [turnId],
        )
      : null;
    const turnStatus = turnRow?.rows[0]?.["status"];

    if (turnId && turnStatus === "result_ready") {
      result = TurnResultSchema.parse(turnRow!.rows[0]!["validated_result"]);
      candidates = validateMemoryCandidates(result.memory);
      proposals = validateIntentionProposals(result.intentions);
      logger.info("резюме хода: применяю сохранённый result_ready без нового LLM", { turnId });
    } else if (turnId && turnStatus === "applied") {
      logger.warn("turn уже применён — закрываю работу без повторов", { turnId });
      await withTransaction(pool, (client) => workQueue.completeWithClient(client, item.id));
      return;
    } else {
      // --- Фаза 1: бюджет (1.9) → контекст из БД (4.4) → модель ---
      budgetOperationId = await reserveModelBudget(item.id, item.attempts);

      const events = turnId
        ? await pool.query<{ text: string | null; actor_id: string | null; actor_name: string | null; raw_update: unknown }>(
            `SELECT text, actor_id, actor_name, raw_update FROM social_events WHERE turn_id = $1 ORDER BY id`,
            [turnId],
          )
        : null;

      const actor = await resolveActor(events?.rows ?? [], scopeId);
      const context = turnId
        ? await assembleContext(events!.rows.map((row) => row.text ?? ""), actor, scopeId)
        : stubContext(item.payload);

      result = await model.runTurn({
        turnId: turnId ?? item.id,
        context,
        signal,
        onUsage: (value) => (usage = value),
      });
      candidates = validateMemoryCandidates(result.memory);
      proposals = validateIntentionProposals(result.intentions);

      if (turnId) {
        // --- Фаза 2: сохранить результат ДО применения (§12.1, 2.7) ---
        await withTransaction(pool, async (client) => {
          await client.query(
            `UPDATE turns SET validated_result = $2::jsonb, status = 'result_ready'
             WHERE id = $1 AND status = 'created'`,
            [turnId, JSON.stringify(result)],
          );
        });
      }
    }

    // --- Фаза 3: применение «всё или ничего» + закрытие работы в той же tx (1.7) ---
    await withTransaction(pool, async (client) => {
      if (turnId) {
        const applied = await client.query(
          `UPDATE turns SET status = 'applied' WHERE id = $1 AND status = 'result_ready' RETURNING id`,
          [turnId],
        );
        if ((applied.rowCount ?? 0) === 0) {
          logger.warn("повторное применение turn отклонено", { turnId });
          await workQueue.completeWithClient(client, item.id);
          return;
        }
        await client.query("UPDATE social_events SET status = 'processed' WHERE turn_id = $1", [turnId]);

        // Доставка: сообщения и/или реакция (§11.2)
        if (result.decision === "answer") {
          await outbox.enqueueMessages(client, turnId, payload.chatId ?? "0", result.messages);
        } else if (result.decision === "react" && result.reaction !== undefined) {
          const target = await reactionTargetMessageId(turnId);
          if (target !== null) {
            await outbox.enqueueReaction(client, turnId, payload.chatId ?? "0", result.reaction, target);
          } else {
            logger.warn("react без целевого сообщения — реакция пропущена", { turnId });
          }
        }

        // Заметки + память
        const noteDeltas = validateDeltas(
          result.notes.map((text) => ({ type: "append_note" as const, text })),
        );
        await applyDeltas(client, item.id, noteDeltas);
        const source = { kind: "journal_event" as const, ref: `turn:${turnId}` };
        for (const candidate of candidates) {
          await memory.insertCandidate(client, candidate, scopeId, source);
        }

        // Намерения (§14.1): предложенные моделью + defer → reply_later (§11.2)
        for (const proposal of proposals) {
          const created = await deps.intentions.create(client, {
            kind: proposal.kind,
            topic: proposal.topic,
            reason: proposal.reason,
            targetChatId: payload.chatId ?? "0",
            delayHours: proposal.delayHours,
            relatedTurnId: turnId,
          });
          if (created === null) {
            logger.debug("намерение-дубликат пропущено (dedupe §14.1)", { turnId, topic: proposal.topic });
          }
        }
        if (result.decision === "defer" && result.defer_hours !== undefined) {
          const batchTopic = (await client.query<{ text: string | null }>(
            "SELECT text FROM social_events WHERE turn_id = $1 AND text IS NOT NULL ORDER BY id DESC LIMIT 1",
            [turnId],
          )).rows[0]?.["text"];
          const deferTopic = (batchTopic ?? "отложенный ответ").slice(0, 80);
          await deps.intentions.create(client, {
            kind: "reply_later",
            topic: deferTopic,
            reason: "decision=defer: персонаж отложил ответ (§11.2)",
            targetChatId: payload.chatId ?? "0",
            delayHours: result.defer_hours,
            relatedTurnId: turnId,
          });
        }

        // Настроение: сжатие дельты + основание заметкой (§7.2)
        if (result.mood_delta) {
          const clamped = clampMoodDelta(result.mood_delta);
          await mood.applyDelta(client, persona.moodBaseline, clamped);
          await applyDeltas(client, item.id, [
            { type: "append_note", text: `mood: ${JSON.stringify(clamped)} — ${result.mood_delta.reason}` },
          ]);
        }

        // Отношения: собеседник-человек, ограниченные дельты + familiarity (§7.3)
        const actor2 = await resolveActor(
          (await client.query<{ actor_id: string | null; actor_name: string | null }>(
            "SELECT actor_id, actor_name FROM social_events WHERE turn_id = $1 AND actor_id IS NOT NULL ORDER BY id LIMIT 1",
            [turnId],
          )).rows,
            scopeId,
          );
        if (actor2.entityId !== null) {
          await relationships.growFamiliarity(client, actor2.entityId, scopeId);
          if (result.relationship_delta) {
            await relationships.applyDelta(client, actor2.entityId, scopeId, {
              affinity: result.relationship_delta.affinity,
              trust: result.relationship_delta.trust,
            });
            // аудит-след пишет СЖАТУЮ дельту — как применено (§7.3)
            const clampStep = (value: number | undefined, limit: number): number =>
              value === undefined ? 0 : Math.max(-limit, Math.min(limit, value));
            await applyDeltas(client, item.id, [
              {
                type: "append_note",
                text: `отношения: ${JSON.stringify({
                  affinity: clampStep(
                    result.relationship_delta.affinity,
                    RELATIONSHIP_LIMITS.affinityPerTurn,
                  ),
                  trust: clampStep(result.relationship_delta.trust, RELATIONSHIP_LIMITS.trustPerTurn),
                })} — ${result.relationship_delta.reason}`,
              },
            ]);
          }
        }

        await workQueue.completeWithClient(client, item.id);
      } else {
        const noteDeltas = validateDeltas(
          result.notes.map((text) => ({ type: "append_note" as const, text })),
        );
        await applyDeltas(client, item.id, noteDeltas);
        await workQueue.completeWithClient(client, item.id);
      }
    });

    // §20: каждый ход логирует IDs, action, latency, usage, бюджет и outcome;
    // основания существенных изменений (mood/отношения) — рядом, полный
    // приватный промпт не логируется
    logger.info("ход выполнен", {
      turnId,
      chatId: payload.chatId ?? null,
      workItemId: item.id,
      decision: result.decision,
      outcome: "applied",
      messages: result.messages.length,
      memoryCandidates: candidates.length,
      latencyMs: Date.now() - startedAtMs,
      ...(usage !== undefined ? { usage } : {}),
      ...(budgetOperationId !== null ? { budgetOperationId } : {}),
      ...(result.mood_delta ? { moodReason: result.mood_delta.reason } : {}),
      ...(result.relationship_delta ? { relationshipReason: result.relationship_delta.reason } : {}),
      reason: result.reason,
    });
  };

  /** Бюджет модельных вызовов (1.9): общий хелпер с intention-обработчиком. */
  function reserveModelBudget(workItemId: string, attempt: number): Promise<string | null> {
    return reserveModelCall(deps.modelBudget, deps.modelDailyCallsLimit, workItemId, attempt);
  }

  /** Карточка человека-собеседника (§7.3) — entity в scope чата. */
  async function resolveActor(
    rows: Array<{ actor_id: string | null; actor_name: string | null }>,
    scopeId: string,
  ): Promise<TurnActor> {
    const row = rows.find((candidate) => candidate.actor_id !== null);
    if (!row || row.actor_id === null) return { entityId: null, name: null };
    const name = row.actor_name ?? `user:${row.actor_id}`;
    const entity = await withTransaction(pool, (client) =>
      memory.entities.findOrCreate(client, "person", name, scopeId),
    );
    return { entityId: entity.id, name };
  }

  /** Сборка контекста хода из БД (§7, §10.4): persona + mood + отношения + память + история. */
  async function assembleContext(
    batchTexts: string[],
    actor: TurnActor,
    scopeId: string,
  ): Promise<ModelTurnContext> {
    // Настроение с затуханием
    const moodNow = await mood.readDecayed(persona.moodBaseline);
    const moodLine = describeMood(moodNow.mood);

    // Отношение к собеседнику
    let relationshipLine: string | undefined;
    if (actor.entityId !== null) {
      const relationship = await relationships.read(actor.entityId, scopeId);
      relationshipLine = describeRelationship(relationship);
    }

    // Память (§10.4): full-text по пачке + недавние; scope-фильтр внутри retrieve
    let memoryBlock: string | undefined;
    try {
      const lastTexts = batchTexts.filter((text) => text.trim().length > 0).slice(-3).join(" ");
      const searched = lastTexts.length > 0 ? await memory.retrieve(scopeId, { textQuery: lastTexts, limit: 6 }) : [];
      const recent = await memory.retrieve(scopeId, { limit: 6 });
      const merged = new Map(searched.concat(recent).map((row) => [row.id, row]));
      const block = [...merged.values()].map((row) => `- (${row.kind}) ${row.text}`).join("\n");
      if (block.length > 0) memoryBlock = block;
    } catch (error) {
      logger.warn("retrieval памяти не удался, ход без карточки памяти", {
        error: error instanceof Error ? error.message : String(error),
      });
    }

    // Последний отправленный ответ (§13.5: только sent — фактический разговор)
    let history: Array<{ role: "user" | "assistant"; content: string }>;
    try {
      const lastSent = await pool.query<{ text: string }>(
        `SELECT payload->>'text' AS text FROM delivery_outbox
         WHERE chat_id = $1 AND status = 'sent' AND payload->>'text' IS NOT NULL
         ORDER BY id DESC LIMIT 2`,
        [scopeId.replace("chat:", "")],
      );
      history = lastSent.rows.reverse().map((row) => ({ role: "assistant" as const, content: row.text }));
    } catch {
      history = [];
    }

    return {
      system: buildSystemPrompt(persona, { moodLine, relationshipLine, memoryBlock }, deps.promptTemplate),
      messages: [...history, ...batchTexts.map((text) => ({ role: "user" as const, content: text || "[сообщение без текста]" }))],
    };
  }

  async function reactionTargetMessageId(turnId: string): Promise<string | null> {
    const result = await pool.query<{ message_id: number | null }>(
      `SELECT (raw_update->'message'->>'message_id')::bigint AS message_id
       FROM social_events WHERE turn_id = $1 AND raw_update->'message'->>'message_id' IS NOT NULL
       ORDER BY id LIMIT 1`,
      [turnId],
    );
    const messageId = result.rows[0]?.["message_id"];
    return messageId === null || messageId === undefined ? null : String(messageId);
  }

  function stubContext(payload: unknown): ModelTurnContext {
    const record = (payload ?? {}) as { messages?: Array<{ role?: string; content?: string }> };
    const messages = Array.isArray(record["messages"])
      ? record["messages"]
          .filter((message) => typeof message?.["content"] === "string")
          .map((message) => ({
            role: message["role"] === "assistant" ? ("assistant" as const) : ("user" as const),
            content: String(message["content"]),
          }))
      : [];
    return {
      system: buildSystemPrompt(persona, {}, deps.promptTemplate),
      messages,
    };
  }
}

export type { Mood };
