import type { Pool } from "pg";
import type { Logger } from "../runtime/logger.js";
import type { WorkHandler } from "../runtime/worker.js";
import { SkipWorkError } from "../runtime/worker.js";
import { withTransaction } from "../runtime/transactions.js";
import { applyDeltas } from "../runtime/deltas.js";
import type { OutboxRepo } from "../telegram/outbox.js";
import type { CharacterModel, ModelTurnContext } from "./model-adapter.js";
import { validateMemoryCandidates } from "../memory/memory-candidates.js";
import type { MemoryRepo } from "../memory/memory-repo.js";
import { chatScope } from "../memory/scope.js";

// Ход по turn (§12.1): события → retrieval → контекст → модель →
// короткая транзакция применения (CAS, outbox, дельты, память).
// Источники памяти назначает runtime; модель только предлагает кандидатов.

export const SYSTEM_PROMPT =
  "Ты — Dio, живой персонаж в Telegram. Отвечай кратко, живо и по-русски. " +
  "Сообщения пользователя идут пачкой одного собеседника — отвечай на пачку целиком.";

const MEMORY_SYSTEM_HEADER =
  "Твоя долговременная память о этом чате (используй естественно; " +
  "не выдумывай сверх записанного и не цитируй формат):";

function stubContextFromPayload(payload: unknown): ModelTurnContext {
  const record = (payload ?? {}) as { messages?: Array<{ role?: string; content?: string }> };
  const messages = Array.isArray(record["messages"])
    ? record["messages"]
        .filter((message) => typeof message?.["content"] === "string")
        .map((message) => ({
          role: message["role"] === "assistant" ? ("assistant" as const) : ("user" as const),
          content: String(message["content"]),
        }))
    : [];
  return { system: SYSTEM_PROMPT, messages };
}

function formatMemoryBlock(items: Array<{ text: string; kind: string; recordedAt: string }>): string {
  return items
    .map((item) => `- (${item.kind}) ${item.text}`)
    .join("\n");
}

export interface TurnHandlerDeps {
  pool: Pool;
  model: CharacterModel | null;
  outbox: OutboxRepo;
  memory: MemoryRepo;
  logger: Logger;
}

export function makeTurnHandler(deps: TurnHandlerDeps): WorkHandler {
  const { pool, model, outbox, memory, logger } = deps;

  return async function handleTurn(item, signal) {
    if (!model) {
      throw new SkipWorkError(
        "модель не настроена",
        "модель не настроена (MODEL_ENDPOINT/API_KEY/MODEL)",
      );
    }
    const payload = (item.payload ?? {}) as { turnId?: string; chatId?: string };
    const turnId = typeof payload.turnId === "string" ? payload.turnId : null;
    const scopeId = typeof payload.chatId === "string" ? chatScope(payload.chatId) : "internal";

    let context: ModelTurnContext;
    let eventTexts: string[] = [];
    if (turnId) {
      const events = await pool.query<{ text: string | null }>(
        "SELECT text FROM social_events WHERE turn_id = $1 ORDER BY id",
        [turnId],
      );
      eventTexts = events.rows.map((row) => row.text ?? "");
      context = {
        system: SYSTEM_PROMPT,
        messages: eventTexts.map((text) => ({
          role: "user" as const,
          content: text || "[сообщение без текста]",
        })),
      };
    } else {
      context = stubContextFromPayload(item.payload);
    }

    // Retrieval (§10.4): сначала по тексту пачки (full-text), затем недавние.
    // Scope-фильтр внутри retrieve: чужие чаты и internal не попадают (§9.2).
    try {
      const lastTexts = eventTexts.filter((text) => text.trim().length > 0).slice(-3).join(" ");
      const searched =
        lastTexts.length > 0 ? await memory.retrieve(scopeId, { textQuery: lastTexts, limit: 6 }) : [];
      const recent = await memory.retrieve(scopeId, { limit: 6 });
      const merged = new Map(searched.concat(recent).map((row) => [row.id, row]));
      const block = formatMemoryBlock([...merged.values()]);
      if (block.length > 0) {
        context = { ...context, system: `${context.system}\n\n${MEMORY_SYSTEM_HEADER}\n${block}` };
      }
    } catch (error) {
      // Память недоступна — ход не срываем: ответ без контекста памяти
      logger.warn("retrieval памяти не удался, ход без карточки памяти", {
        turnId,
        error: error instanceof Error ? error.message : String(error),
      });
    }

    const result = await model.runTurn({ turnId: turnId ?? item.id, context, signal });

    // Кандидаты памяти проверяются ДО применения (§10.1): что не прошло —
    // отклонено целиком, событие остаётся в журнале
    const candidates = validateMemoryCandidates(result.memory);

    await withTransaction(pool, async (client) => {
      if (turnId) {
        // CAS: применить turn не более одного раза (§12.1, инвариант 4)
        const applied = await client.query(
          "UPDATE turns SET status = 'applied' WHERE id = $1 AND status = 'created' RETURNING id",
          [turnId],
        );
        if ((applied.rowCount ?? 0) === 0) {
          logger.warn("повторное применение turn отклонено", { turnId });
          return;
        }
        await client.query("UPDATE social_events SET status = 'processed' WHERE turn_id = $1", [turnId]);
        await outbox.enqueueMessages(client, turnId, payload.chatId ?? "0", result.messages);
        await applyDeltas(
          client,
          item.id,
          result.notes.map((text) => ({ type: "append_note" as const, text })),
        );
        // Источник один на ход: journal_event → turn. Runtime назначает сам.
        const source = { kind: "journal_event" as const, ref: `turn:${turnId}` };
        for (const candidate of candidates) {
          await memory.insertCandidate(client, candidate, scopeId, source);
        }
      } else {
        await applyDeltas(
          client,
          item.id,
          result.notes.map((text) => ({ type: "append_note" as const, text })),
        );
      }
    });

    logger.info("ход выполнен", {
      turnId,
      decision: result.decision,
      messages: result.messages.length,
      memoryCandidates: candidates.length,
      reason: result.reason,
    });
  };
}
