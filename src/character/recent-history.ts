import type { Pool } from "pg";

// 8.1: недавний разговор в контексте хода — сообщения обеих сторон из БД
// (журнал входящих + отправленные ответы), не только память и не пара
// последних реплик. §13.5: в разговор попадают только доставленные ответы.

export interface HistoryMessage {
  role: "user" | "assistant";
  content: string;
}

/** Последние limit сообщений чата (обе стороны) в хронологическом порядке.
 *  excludeTurnId — события текущего хода: они уже переданы пачкой, дублировать их нельзя. */
export async function recentHistory(
  pool: Pool,
  chatId: string,
  limit: number,
  excludeTurnId?: string,
): Promise<HistoryMessage[]> {
  const rows = await pool.query<{ role: "user" | "assistant"; content: string }>(
    `SELECT role, content FROM (
       SELECT 'user' AS role, text AS content, occurred_at AS ts
       FROM social_events
       WHERE chat_id = $1 AND text IS NOT NULL AND kind IN ('message', 'reply', 'mention')
         AND ($3::bigint IS NULL OR turn_id IS NULL OR turn_id <> $3::bigint)
       UNION ALL
       SELECT 'assistant' AS role, payload->>'text' AS content, created_at AS ts
       FROM delivery_outbox
       WHERE chat_id = $1 AND status = 'sent' AND payload->>'text' IS NOT NULL
         AND ($3::bigint IS NULL OR turn_id <> $3::bigint)
     ) timeline
     ORDER BY ts DESC
     LIMIT $2`,
    [chatId, limit, excludeTurnId ?? null],
  );
  return rows.rows.reverse();
}

/** Подгонка истории под бюджет объёма: первыми теряются самые старые сообщения. */
export function fitHistory(messages: HistoryMessage[], maxChars: number): HistoryMessage[] {
  const out: HistoryMessage[] = [];
  let total = 0;
  for (let i = messages.length - 1; i >= 0; i--) {
    const message = messages[i]!;
    if (out.length > 0 && total + message.content.length > maxChars) break;
    total += message.content.length;
    out.unshift(message);
  }
  return out;
}
