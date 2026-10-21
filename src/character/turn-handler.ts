import type { Pool } from "pg";
import type { Logger } from "../runtime/logger.js";
import type { WorkHandler } from "../runtime/worker.js";
import { SkipWorkError } from "../runtime/worker.js";
import { withTransaction } from "../runtime/transactions.js";
import { applyDeltas } from "../runtime/deltas.js";
import type { OutboxRepo } from "../telegram/outbox.js";
import type { CharacterModel, ModelTurnContext } from "./model-adapter.js";

// Ход по turn (§12.1): события → контекст → модель → короткая транзакция
// применения (CAS однократности, outbox, дельты). Выделен из index.ts,
// чтобы связка была тестируемой (находка Н3 приёмки этапа 2).

export const SYSTEM_PROMPT =
  "Ты — Dio, живой персонаж в Telegram. Отвечай кратко, живо и по-русски. " +
  "Сообщения пользователя идут пачкой одного собеседника — отвечай на пачку целиком.";

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

export interface TurnHandlerDeps {
  pool: Pool;
  model: CharacterModel | null;
  outbox: OutboxRepo;
  logger: Logger;
}

export function makeTurnHandler(deps: TurnHandlerDeps): WorkHandler {
  const { pool, model, outbox, logger } = deps;

  return async function handleTurn(item, signal) {
    if (!model) {
      throw new SkipWorkError(
        "модель не настроена",
        "модель не настроена (MODEL_ENDPOINT/API_KEY/MODEL)",
      );
    }
    const payload = (item.payload ?? {}) as { turnId?: string; chatId?: string };
    const turnId = typeof payload.turnId === "string" ? payload.turnId : null;

    let context: ModelTurnContext;
    if (turnId) {
      const events = await pool.query<{ text: string | null }>(
        "SELECT text FROM social_events WHERE turn_id = $1 ORDER BY id",
        [turnId],
      );
      context = {
        system: SYSTEM_PROMPT,
        messages: events.rows.map((row) => ({
          role: "user" as const,
          content: row.text ?? "[сообщение без текста]",
        })),
      };
    } else {
      context = stubContextFromPayload(item.payload);
    }

    const result = await model.runTurn({ turnId: turnId ?? item.id, context, signal });

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
      reason: result.reason,
    });
  };
}
