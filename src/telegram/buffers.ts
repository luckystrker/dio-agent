import type { Pool } from "pg";
import type { Logger } from "../runtime/logger.js";
import { enqueueWithClient } from "../runtime/work-queue.js";
import type { PendingEvent } from "./polling.js";

// Буферы чатов (§6.2): собирают сообщения в порцию, пока пользователь печатает.
// Окна из спецификации; закрытие — по idle (тишина), по max (от первого события),
// по 20 событиям или лимиту текста. Никакого await sleep в ingest-пути:
// только таймеры. In-memory таймер можно потерять — принадлежность событий
// созданному turn записывается транзакционно и переживает падение.

export interface BufferWindows {
  idleMs: number;
  maxMs: number;
}

const WINDOWS = {
  /** Личка: 1–2 с idle, максимум 3 с */
  direct: { idleMs: 1_500, maxMs: 3_000 },
  /** Пассивная группа: 3–5 с idle, максимум 6 с */
  passiveGroup: { idleMs: 4_000, maxMs: 6_000 },
  /** Reply / mention / command: 0–300 мс idle, максимум 1 с */
  fast: { idleMs: 150, maxMs: 1_000 },
} as const satisfies Record<string, BufferWindows>;

export const MAX_EVENTS_PER_BATCH = 20;
export const MAX_BATCH_TEXT_CHARS = 8_000;

export function windowsFor(event: Pick<PendingEvent, "chatType" | "kind">): BufferWindows {
  if (event.chatType === "private") return { ...WINDOWS.direct };
  if (event.kind === "reply" || event.kind === "mention" || event.kind === "command") {
    return { ...WINDOWS.fast };
  }
  return { ...WINDOWS.passiveGroup };
}

interface BufferState {
  chatId: string;
  eventIds: string[];
  textChars: number;
  windows: BufferWindows;
  idleTimer?: NodeJS.Timeout;
  maxTimer?: NodeJS.Timeout;
  closing: boolean;
}

export type EnqueueWork = (client: import("pg").PoolClient, kind: "direct" | "group", payload: Record<string, unknown>) => Promise<string>;

export class ChatBufferManager {
  private readonly buffers = new Map<string, BufferState>();

  constructor(
    private readonly deps: {
      pool: Pool;
      logger: Logger;
      enqueueWork?: EnqueueWork;
    },
  ) {}

  /** Новое pending-событие: в буфер чата, таймеры пересчитываются. */
  add(event: PendingEvent): void {
    let state = this.buffers.get(event.chatId);
    if (!state) {
      state = {
        chatId: event.chatId,
        eventIds: [],
        textChars: 0,
        windows: windowsFor(event),
        closing: false,
      };
      this.buffers.set(event.chatId, state);
      state.maxTimer = setTimeout(() => void this.close(event.chatId), state.windows.maxMs);
      this.deps.logger.debug("буфер открыт", { chatId: event.chatId, windows: state.windows });
    }

    state.eventIds.push(event.eventId);
    state.textChars += event.text?.length ?? 0;

    if (state.eventIds.length >= MAX_EVENTS_PER_BATCH || state.textChars >= MAX_BATCH_TEXT_CHARS) {
      void this.close(event.chatId);
      return;
    }
    this.rescheduleIdle(state);
  }

  /** Graceful shutdown: закрыть все буферы немедленно. */
  async flushAll(): Promise<void> {
    const chats = [...this.buffers.keys()];
    for (const chatId of chats) {
      await this.close(chatId);
    }
  }

  /**
   * Recovery (§12.2 «событие pending, turn ещё не создан — восстановить буфер»):
   * таймеры потеряны при падении, pending-события закрываются в turn немедленно.
   */
  async recoverStalePending(): Promise<{ chats: number; events: number }> {
    const result = await this.deps.pool.query<{ chat_id: string; ids: string[] }>(
      `SELECT chat_id, array_agg(id::text) AS ids
       FROM social_events WHERE status = 'pending'
       GROUP BY chat_id`,
    );
    let events = 0;
    for (const row of result.rows) {
      const chatTypeRow = await this.deps.pool.query<{ chat_type: string }>(
        "SELECT chat_type FROM social_events WHERE chat_id = $1 LIMIT 1",
        [row.chat_id],
      );
      const chatType = chatTypeRow.rows[0]?.["chat_type"] === "private" ? "private" : "group";
      events += row.ids.length;
      await this.createTurn(chatType === "private" ? "direct" : "group", row.chat_id, row.ids);
    }
    return { chats: result.rows.length, events };
  }

  private rescheduleIdle(state: BufferState): void {
    if (state.idleTimer) clearTimeout(state.idleTimer);
    state.idleTimer = setTimeout(() => void this.close(state.chatId), state.windows.idleMs);
  }

  private async close(chatId: string): Promise<void> {
    const state = this.buffers.get(chatId);
    if (!state || state.closing) return;
    state.closing = true;
    if (state.idleTimer) clearTimeout(state.idleTimer);
    if (state.maxTimer) clearTimeout(state.maxTimer);
    this.buffers.delete(chatId);

    if (state.eventIds.length === 0) return;
    const chatType = await this.readChatType(state.eventIds[0]!);
    try {
      await this.createTurn(chatType === "private" ? "direct" : "group", chatId, state.eventIds);
    } catch (error) {
      this.deps.logger.error("не удалось закрыть буфер в turn", {
        chatId,
        events: state.eventIds.length,
        error: error instanceof Error ? error.message : String(error),
      });
    }
  }

  private async readChatType(eventId: string): Promise<"private" | "group"> {
    const result = await this.deps.pool.query<{ chat_type: string }>(
      "SELECT chat_type FROM social_events WHERE id = $1",
      [eventId],
    );
    return result.rows[0]?.["chat_type"] === "private" ? "private" : "group";
  }

  /**
   * Транзакционное закрытие порции (§6.2): turn с постоянным ID,
   * привязка событий, постановка работы — атомарно.
   */
  private async createTurn(
    kind: "direct" | "group",
    chatId: string,
    eventIds: string[],
  ): Promise<void> {
    const client = await this.deps.pool.connect();
    try {
      await client.query("BEGIN");
      const turn = await client.query<{ id: string }>(
        "INSERT INTO turns (chat_id) VALUES ($1) RETURNING id",
        [chatId],
      );
      const turnId = String(turn.rows[0]!.id);
      await client.query(
        "UPDATE social_events SET turn_id = $1, status = 'buffered' WHERE id = ANY($2::bigint[]) AND status = 'pending'",
        [turnId, eventIds],
      );
      const workItem = await (this.deps.enqueueWork ?? defaultEnqueueWork)(client, kind, { turnId, chatId });
      await client.query("COMMIT");
      this.deps.logger.info("порция закрыта в turn", { chatId, turnId, events: eventIds.length, workItem });
    } catch (error) {
      await client.query("ROLLBACK").catch(() => undefined);
      throw error;
    } finally {
      client.release();
    }
  }
}

const defaultEnqueueWork: EnqueueWork = (client, kind, payload) =>
  enqueueWithClient(client, kind, payload);
