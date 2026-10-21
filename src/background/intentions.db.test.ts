// Интеграционный тест (живой PostgreSQL, база dio_test).
// Этап 5 «Фон» — IntentionRepo (§14.1–§14.3):
// - create: dedupe по (kind, topic, chat) — повторная та же идея → null,
//   нормализация регистра/пробелов темы не размножает намерение (§14.1);
// - claimDue: до срока не берёт, просроченный (expires_at) не берёт,
//   повторный вызов не ставит в работу второй раз (CAS статуса + работа
//   kind=intention в той же транзакции, §14.2);
// - expiresAt = notBefore + 72ч (INTENTION_TTL_HOURS, §14.1);
// - proactiveAllowed: ≤2/день, ≥4ч, группы off (§14.3);
// - exploreCompletedToday: только completed за сегодня (§16).

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { IntentionRepo } from "./intentions.js";
import { INTENTION_TTL_HOURS } from "./intention-proposals.js";
import { withTransaction } from "../runtime/transactions.js";
import { createTestPool } from "../test-support/db.js";

let pool: Pool;
let repo: IntentionRepo;

beforeEach(async () => {
  pool = createTestPool();
  repo = new IntentionRepo(pool);
  // intentions/proactive_log не каскадятся из cleanTelegramTables — чистим сами
  await pool.query(
    "TRUNCATE TABLE proactive_log, intentions, work_items, budget_reservations RESTART IDENTITY CASCADE",
  );
});

afterEach(async () => {
  await pool.end();
});

async function create(input: Partial<Parameters<IntentionRepo["create"]>[1]> = {}) {
  return withTransaction(pool, (client) =>
    repo.create(client, {
      kind: "check_in",
      topic: "ремонт кухни",
      reason: "обещал спросить",
      targetChatId: "1000",
      delayHours: -0.01, // not_before в прошлом — сразу due (репо не валидирует delay)
      ...input,
    }),
  );
}

async function rows<T extends import("pg").QueryResultRow>(
  query: string,
  params: unknown[] = [],
): Promise<T[]> {
  return (await pool.query<T>(query, params)).rows;
}

// ---------- create: dedupe §14.1 ----------

describe("IntentionRepo.create (§14.1)", () => {
  it("создаёт намерение pending с not_before из delay и related_turn_id", async () => {
    const created = await create({ delayHours: 2, relatedTurnId: "77" });
    expect(created).not.toBeNull();
    expect(created!.status).toBe("pending");
    expect(created!.kind).toBe("check_in");
    expect(created!.targetChatId).toBe("1000");
    expect(created!.relatedTurnId).toBe("77");
    const notBeforeIn = (created!.notBefore.getTime() - Date.now()) / 3_600_000;
    expect(notBeforeIn).toBeGreaterThan(1.9);
    expect(notBeforeIn).toBeLessThan(2.1);
  });

  it("повтор той же идеи (kind+topic+chat) → null, в таблице одна строка", async () => {
    const first = await create();
    const second = await create({ reason: "другая формулировка причины" });
    expect(first).not.toBeNull();
    expect(second).toBeNull(); // ON CONFLICT (dedupe_key) DO NOTHING
    expect(await rows("SELECT count(*)::int AS c FROM intentions")).toEqual([{ c: 1 }]);
  });

  it("регистр и внешние пробелы темы не размножают намерение (нормализация dedupe_key)", async () => {
    await create({ topic: "ремонт кухни" });
    const variant = await create({ topic: "  РЕМОНТ Кухни  " });
    expect(variant).toBeNull();
    expect(await rows("SELECT count(*)::int AS c FROM intentions")).toEqual([{ c: 1 }]);
  });

  it("другой вид, другая тема или другой чат — отдельные намерения", async () => {
    await create({ topic: "горы" });
    expect(await create({ topic: "горы", kind: "explore" })).not.toBeNull(); // другой kind
    expect(await create({ topic: "море" })).not.toBeNull(); // другая тема
    expect(await create({ topic: "горы", targetChatId: "1001" })).not.toBeNull(); // другой чат
    expect(await rows("SELECT count(*)::int AS c FROM intentions")).toEqual([{ c: 4 }]);
  });

  it("expires_at = not_before + 72 часа (INTENTION_TTL_HOURS, §14.1)", async () => {
    const created = await create({ delayHours: 1 });
    const ttlHours =
      (created!.expiresAt!.getTime() - created!.notBefore.getTime()) / 3_600_000;
    expect(ttlHours).toBeCloseTo(INTENTION_TTL_HOURS, 6);
    expect(INTENTION_TTL_HOURS).toBe(72);
  });
});

// ---------- claimDue §14.2 ----------

describe("IntentionRepo.claimDue (§14.2)", () => {
  it("до срока (not_before в будущем) не берёт: 0, статус pending, работы нет", async () => {
    await create({ delayHours: 1 });
    expect(await repo.claimDue()).toBe(0);
    const intention = (await rows("SELECT status FROM intentions"))[0]!;
    expect(intention.status).toBe("pending");
    expect(await rows("SELECT count(*)::int AS c FROM work_items")).toEqual([{ c: 0 }]);
  });

  it("due-намерение переводится в queued и ставится в работу kind=intention", async () => {
    const created = await create();
    expect(await repo.claimDue()).toBe(1);
    const after = await repo.byId(created!.id);
    expect(after!.status).toBe("queued");
    const work = await rows<{ kind: string; payload: { intentionId?: string } }>(
      "SELECT kind, payload FROM work_items",
    );
    expect(work).toEqual([{ kind: "intention", payload: { intentionId: created!.id } }]);
  });

  it("двойной claimDue: второй вызов возвращает 0 — двойная постановка невозможна", async () => {
    await create();
    expect(await repo.claimDue()).toBe(1);
    expect(await repo.claimDue()).toBe(0);
    expect(await rows("SELECT count(*)::int AS c FROM work_items")).toEqual([{ c: 1 }]);
  });

  it("просроченное (expires_at в прошлом) не берёт — остаётся pending (отмена в хендлере)", async () => {
    const created = await create();
    await pool.query("UPDATE intentions SET expires_at = now() - interval '1 minute' WHERE id = $1", [
      created!.id,
    ]);
    expect(await repo.claimDue()).toBe(0);
    expect((await repo.byId(created!.id))!.status).toBe("pending");
    expect(await rows("SELECT count(*)::int AS c FROM work_items")).toEqual([{ c: 0 }]);
  });

  it("limit ограничивает выборку, остаток остаётся pending (порциями, §5.2)", async () => {
    await create({ topic: "a" });
    await create({ topic: "b" });
    await create({ topic: "c" });
    expect(await repo.claimDue(2)).toBe(2);
    const statuses = await rows<{ status: string; c: number }>(
      "SELECT status, count(*)::int AS c FROM intentions GROUP BY status",
    );
    expect(statuses).toEqual(expect.arrayContaining([
      { status: "queued", c: 2 },
      { status: "pending", c: 1 },
    ]));
  });
});

// ---------- переходы статусов ----------

describe("IntentionRepo: markRunning/complete/cancel", () => {
  it("markRunning переводит queued → running; повтор (уже running) — без ошибки", async () => {
    const created = await create();
    await repo.claimDue();
    await repo.markRunning(created!.id);
    expect((await repo.byId(created!.id))!.status).toBe("running");
    await expect(repo.markRunning(created!.id)).resolves.toBeUndefined();
    expect((await repo.byId(created!.id))!.status).toBe("running");
  });

  it("cancel пишет причину с префиксом «отменено:» (§14.3: отмена объясняется в журнале)", async () => {
    const created = await create();
    await repo.cancel(created!.id, "срок истёк");
    const after = await repo.byId(created!.id);
    expect(after!.status).toBe("cancelled");
    expect(after!.reason).toContain("отменено:");
    expect(after!.reason).toContain("срок истёк");
  });
});

// ---------- proactive-политика §14.3 ----------

describe("IntentionRepo.proactiveAllowed (§14.3)", () => {
  it("личный чат без отправок — разрешено", async () => {
    expect(await repo.proactiveAllowed("1000", true)).toBe(true);
  });

  it("2 отправки сегодня — запрет", async () => {
    await pool.query(
      "INSERT INTO proactive_log (chat_id, sent_at) VALUES (1000, now()), (1000, now() - interval '5 hours')",
    );
    expect(await repo.proactiveAllowed("1000", true)).toBe(false);
  });

  it("последняя отправка меньше 4 часов назад — запрет", async () => {
    await pool.query(
      "INSERT INTO proactive_log (chat_id, sent_at) VALUES (1000, now() - interval '1 hour')",
    );
    expect(await repo.proactiveAllowed("1000", true)).toBe(false);
  });

  it("последняя отправка 4+ часа назад и меньше 2 за сегодня — разрешено", async () => {
    await pool.query(
      "INSERT INTO proactive_log (chat_id, sent_at) VALUES (1000, now() - interval '5 hours')",
    );
    expect(await repo.proactiveAllowed("1000", true)).toBe(true);
  });

  it("группа — всегда запрет, даже без истории отправок", async () => {
    expect(await repo.proactiveAllowed("1000", false)).toBe(false);
  });

  it("лимит считается по чату: чужие отправки не мешают", async () => {
    await pool.query(
      "INSERT INTO proactive_log (chat_id, sent_at) VALUES (1001, now()), (1001, now() - interval '1 hour')",
    );
    expect(await repo.proactiveAllowed("1000", true)).toBe(true);
  });

  it("logProactive фиксирует chat_id и intention_id", async () => {
    const created = await create();
    await withTransaction(pool, (client) => repo.logProactive(client, "1000", created!.id));
    const log = await rows<{ chat_id: string; intention_id: string }>(
      "SELECT chat_id::text, intention_id::text FROM proactive_log",
    );
    expect(log).toEqual([{ chat_id: "1000", intention_id: created!.id }]);
  });
});

// ---------- §16: счётчик exploration ----------

describe("IntentionRepo.exploreCompletedToday (§16: до 2 exploration в сутки)", () => {
  it("пусто → 0", async () => {
    expect(await repo.exploreCompletedToday()).toBe(0);
  });

  it("считает только completed-за-сегодня: вчерашнее completed и сегодняшнее cancelled/pending не считаются", async () => {
    await pool.query(`
      INSERT INTO intentions (kind, target_chat_id, topic, reason, status, not_before, updated_at, dedupe_key)
      VALUES
        ('explore', 1000, 'вчерашнее', 'r', 'completed', now(), now() - interval '1 day', 'fixture:explore:вчерашнее:1000'),
        ('explore', 1000, 'отменённое', 'r', 'cancelled', now(), now(), 'fixture:explore:отменённое:1000'),
        ('explore', 1000, 'в процессе', 'r', 'pending', now(), now(), 'fixture:explore:в процессе:1000'),
        ('check_in', 1000, 'не explore', 'r', 'completed', now(), now(), 'fixture:check_in:не explore:1000')`);
    expect(await repo.exploreCompletedToday()).toBe(0);

    await pool.query(`
      INSERT INTO intentions (kind, target_chat_id, topic, reason, status, not_before, updated_at, dedupe_key)
      VALUES ('explore', 1000, 'сегодняшнее', 'r', 'completed', now(), now(), 'fixture:explore:сегодняшнее:1000')`);
    expect(await repo.exploreCompletedToday()).toBe(1);
  });
});
