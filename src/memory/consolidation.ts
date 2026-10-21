import type { Pool } from "pg";
import { withTransaction } from "../runtime/transactions.js";
import { MemoryRepo } from "./memory-repo.js";
import { chatScope } from "./scope.js";

// Консолидация (§10.2): фоновая уборка памяти. MVP-срез этапа 3 —
// детерминированные episode summaries processed-событий, закрывшихся
// более 4 часов назад (граница эпизода DM). LLM-сводки — после бюджетов
// модельных вызовов (чек-лист 1.9). Дублей не создаёт: отметка
// summarized_at ставится в той же транзакции, что и запись памяти.

const EPISODE_SILENCE_HOURS = 4;

interface StaleEvent {
  id: string;
  chat_id: string;
  text: string | null;
  occurred_at: Date;
}

export async function consolidateEpisodes(
  pool: Pool,
): Promise<{ chats: number; events: number }> {
  const memory = new MemoryRepo(pool);

  const stale = await pool.query<StaleEvent>(
    `SELECT id::text, chat_id::text, text, occurred_at
     FROM social_events
     WHERE status = 'processed' AND summarized_at IS NULL
       AND occurred_at < now() - ($1::int * interval '1 hour')
     ORDER BY chat_id, id`,
    [EPISODE_SILENCE_HOURS],
  );
  if (stale.rows.length === 0) return { chats: 0, events: 0 };

  const byChat = new Map<string, StaleEvent[]>();
  for (const event of stale.rows) {
    const list = byChat.get(event.chat_id) ?? [];
    list.push(event);
    byChat.set(event.chat_id, list);
  }

  let events = 0;
  await withTransaction(pool, async (client) => {
    for (const [chatId, chatEvents] of byChat) {
      const first = chatEvents[0]!;
      const last = chatEvents[chatEvents.length - 1]!;
      const from = first.occurred_at.toISOString().slice(0, 16).replace("T", " ");
      const to = last.occurred_at.toISOString().slice(0, 16).replace("T", " ");
      const firstText = (first.text ?? "").slice(0, 80);
      const lastText = (last.text ?? "").slice(0, 80);
      const summaryText =
        chatEvents.length === 1
          ? `Эпизод чата ${chatId} (${from}): «${firstText}»`
          : `Эпизод чата ${chatId} с ${from} по ${to}: ${chatEvents.length} сообщений. ` +
            `Начало: «${firstText}». Финал: «${lastText}»`;

      await memory.insertCandidate(
        client,
        {
          kind: "experience",
          text: summaryText,
          entities: [],
          salience: 0.3,
        },
        chatScope(chatId),
        {
          kind: "runtime_action",
          ref: `episode:${chatId}:${last.id}`,
        },
      );
      await client.query("UPDATE social_events SET summarized_at = now() WHERE id = ANY($1::bigint[])", [
        chatEvents.map((event) => event.id),
      ]);
      events += chatEvents.length;
    }
  });

  return { chats: byChat.size, events };
}
