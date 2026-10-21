// Интеграционный тест (живой PostgreSQL, база dio_test): консолидация,
// MVP-срез §10.2 — детерминированные episode summaries processed-событий,
// закрывшихся более 4 часов назад (граница эпизода DM). Сводка — experience
// в chat-scope с источником runtime_action; summarized_at ставится в той же
// транзакции, поэтому повторный прогон не создаёт дублей.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { consolidateEpisodes } from "./consolidation.js";
import { cleanMemoryTables, createTestPool } from "../test-support/db.js";
import { cleanTelegramTables } from "../test-support/telegram-db.js";

let pool: Pool;
let nextUpdateId = 9_000;

beforeEach(async () => {
  pool = createTestPool();
  await cleanTelegramTables(pool);
  await cleanMemoryTables(pool);
  nextUpdateId = 9_000;
});

afterEach(async () => {
  // не оставляем записи следующим db-файлам: порядок файлов не гарантирован
  await cleanMemoryTables(pool);
  await pool.end();
});

interface EventSpec {
  chatId: string;
  text: string;
  /** Дробные часы допустимы: 3.9/4.5 для точечной проверки границы эпизода. */
  hoursAgo: number;
  status?: string;
}

/** Прямая вставка social_events с заданным occurred_at (как сделал бы ingest). */
async function seedEvent(spec: EventSpec): Promise<string> {
  const result = await pool.query<{ id: string }>(
    `INSERT INTO social_events
       (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status, occurred_at)
     VALUES ($1, $2, 'private', '42', 'message', $3, '{}', $4, now() - ($5::numeric * interval '1 hour'))
     RETURNING id::text`,
    [nextUpdateId++, spec.chatId, spec.text, spec.status ?? "processed", spec.hoursAgo],
  );
  return result.rows[0]!.id;
}

interface SummaryRow {
  text: string;
  scope_id: string;
  kind: string;
  salience: string;
}

async function episodeSummaries(): Promise<SummaryRow[]> {
  const result = await pool.query<SummaryRow>(
    "SELECT text, scope_id, kind, salience::text FROM memory_items WHERE kind = 'experience'",
  );
  return result.rows;
}

describe("consolidateEpisodes (§10.2)", () => {
  it("processed-события старше 4ч → experience в chat-scope, источник runtime_action, summarized_at проставлен", async () => {
    const first100 = await seedEvent({ chatId: "100", text: "начали обсуждать отпуск", hoursAgo: 6 });
    const last100 = await seedEvent({ chatId: "100", text: "решили ехать в горы", hoursAgo: 5 });
    const only200 = await seedEvent({ chatId: "200", text: "короткий вопрос", hoursAgo: 5 });

    const report = await consolidateEpisodes(pool);

    expect(report).toEqual({ chats: 2, events: 3 });

    const summaries = await episodeSummaries();
    expect(summaries).toHaveLength(2);
    const chat100 = summaries.find((row) => row.scope_id === "chat:100")!;
    const chat200 = summaries.find((row) => row.scope_id === "chat:200")!;
    // сводка чата со многими событиями считает их и цитирует крайние
    expect(chat100.text).toContain("2 сообщений");
    expect(chat100.text).toContain("начали обсуждать отпуск");
    expect(chat100.text).toContain("решили ехать в горы");
    expect(chat200.text).toContain("короткий вопрос");
    expect(new Set(summaries.map((row) => row.kind))).toEqual(new Set(["experience"]));

    // источник сводки — runtime_action, ref привязан к последнему событию чата
    const sources = await pool.query<{ kind: string; ref: string }>(
      `SELECT DISTINCT s.kind, s.ref FROM memory_sources ms
       JOIN sources s ON s.id = ms.source_id
       JOIN memory_items m ON m.id = ms.memory_id
       WHERE m.kind = 'experience' ORDER BY s.ref`,
    );
    expect(sources.rows).toEqual([
      { kind: "runtime_action", ref: `episode:100:${last100}` },
      { kind: "runtime_action", ref: `episode:200:${only200}` },
    ]);

    // summarized_at проставлен ровно у обработанных
    const summarized = await pool.query<{ id: string }>(
      "SELECT id::text FROM social_events WHERE summarized_at IS NOT NULL ORDER BY id",
    );
    expect(summarized.rows.map((row) => row.id).sort()).toEqual(
      [first100, last100, only200].sort(),
    );
  });

  it("повторный прогон — ноль новых записей (идемпотентность)", async () => {
    await seedEvent({ chatId: "100", text: "тема", hoursAgo: 6 });
    const first = await consolidateEpisodes(pool);
    expect(first.events).toBe(1);

    const before = await episodeSummaries();
    const second = await consolidateEpisodes(pool);
    expect(second).toEqual({ chats: 0, events: 0 });
    expect(await episodeSummaries()).toEqual(before);
  });

  it("свежие (<4ч), pending и journal_only события не трогаются", async () => {
    await seedEvent({ chatId: "300", text: "свежее processed", hoursAgo: 1 });
    await seedEvent({ chatId: "301", text: "старое pending", hoursAgo: 6, status: "pending" });
    await seedEvent({ chatId: "302", text: "старое journal_only", hoursAgo: 6, status: "journal_only" });
    await seedEvent({ chatId: "303", text: "молодое processed", hoursAgo: 3 });

    const report = await consolidateEpisodes(pool);
    expect(report).toEqual({ chats: 0, events: 0 });
    expect(await episodeSummaries()).toEqual([]);

    const untouched = await pool.query<{ count: string }>(
      "SELECT count(*)::text FROM social_events WHERE summarized_at IS NOT NULL",
    );
    expect(untouched.rows[0]!.count).toBe("0");

    // journal_only не подхватится и позже: статус ≠ processed — навсегда вне сводок
    await pool.query("UPDATE social_events SET occurred_at = now() - interval '10 hour'");
    const afterAging = await consolidateEpisodes(pool);
    expect(afterAging).toEqual({ chats: 2, events: 2 }); // pending и journal_only не вошли
  });

  // Спека §10.2: граница — СТРОГО более 4 часов («4 часа тишины в DM»).
  // Замечание критика «4.5ч остаётся не тронутым» проверено фактическим
  // прогоном предиката (occurred_at < now() - 4h): 4.5ч-событие предикат
  // ВЫБИРАЕТ — тишина уже превысила границу, сводка положена. Поэтому
  // двусторонняя проверка: 3.9ч — ещё нет, 4.5ч — уже да.
  it("граница эпизода точная: 3.9ч — ещё нет, 4.5ч — уже да (строгое «более 4 часов»)", async () => {
    const inside = await seedEvent({ chatId: "401", text: "почти четыре часа тишины", hoursAgo: 3.9 });
    const outside = await seedEvent({ chatId: "402", text: "четыре с половиной часа тишины", hoursAgo: 4.5 });

    const report = await consolidateEpisodes(pool);
    expect(report).toEqual({ chats: 1, events: 1 });

    // сводка ровно одна — у чата за границей
    const summaries = await episodeSummaries();
    expect(summaries).toHaveLength(1);
    expect(summaries[0]!.scope_id).toBe("chat:402");
    expect(summaries[0]!.text).toContain("четыре с половиной часа тишины");

    // 3.9ч-событие не тронуто: summarized_at NULL, записей памяти нет
    const flags = await pool.query<{ id: string; summarized_at: Date | null }>(
      "SELECT id::text, summarized_at FROM social_events WHERE id = ANY($1::bigint[])",
      [[inside, outside]],
    );
    const byId = new Map(flags.rows.map((row) => [row.id, row.summarized_at]));
    expect(byId.get(inside)).toBeNull();
    expect(byId.get(outside)).not.toBeNull();
  });
});
