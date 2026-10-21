import { afterAll, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { withTransaction } from "../runtime/transactions.js";

async function makeTopic(name: string): Promise<string> {
  const entity = await withTransaction(pool, (client) =>
    memory.entities.findOrCreate(client, "topic", name, SCOPE),
  );
  return entity.id;
}
import { MemoryRepo } from "../memory/memory-repo.js";
import { InitiativeRepo, INITIATIVE_SUPPRESSION_DAYS } from "./initiative.js";
import { chatScope } from "../memory/scope.js";
import { createTestPool, cleanMemoryTables } from "../test-support/db.js";

// 4.6: подавление повторной инициативы по теме. Прямой вопрос обходит
// подавление ВСЕГДА: механизм не участвует в retrieval (§10.4).

const pool: Pool = await createTestPool();
const initiative = new InitiativeRepo(pool);
const memory = new MemoryRepo(pool);
const SCOPE = chatScope("900");

beforeEach(async () => {
  await cleanMemoryTables(pool);
  await pool.query("DELETE FROM initiative_topics");
});

describe("InitiativeRepo (§2, §10.4)", () => {
  it("без записи темы — инициатива не подавлена", async () => {
    const topic = await makeTopic("горы");
    expect(await initiative.isSuppressed(topic, SCOPE)).toBe(false);
  });

  it("запись инициативы подавляет тему на срок SUPPRESSION_DAYS", async () => {
    const topic = await makeTopic("горы");
    const other = await makeTopic("море");
    await initiative.record(topic, SCOPE);
    expect(await initiative.isSuppressed(topic, SCOPE)).toBe(true);
    // в другом чате подавление не действует
    expect(await initiative.isSuppressed(topic, chatScope("901"))).toBe(false);
    // другая тема в том же чате не подавлена
    expect(await initiative.isSuppressed(other, SCOPE)).toBe(false);
    expect(INITIATIVE_SUPPRESSION_DAYS).toBeGreaterThanOrEqual(1);
  });

  it("повторная запись обновляет срок", async () => {
    const topic = await makeTopic("горы");
    await initiative.record(topic, SCOPE);
    await initiative.record(topic, SCOPE); // идемпотентность upsert
    expect(await initiative.isSuppressed(topic, SCOPE)).toBe(true);
  });

  it("ПОДАВЛЕНИЕ НЕ ВЛИЯЕТ НА RETRIEVAL: прямой вопрос получает память подавленной темы (§10.4)", async () => {
    // память о теме + тема подавлена инициативой
    await withTransaction(pool, (client) =>
      memory.insertCandidate(
        client,
        { kind: "claim", text: "Пользователь любит горы", entities: [], salience: 0.8 },
        SCOPE,
        { kind: "journal_event", ref: "turn:test-init" },
      ),
    );
    const entity = await withTransaction(pool, (client) =>
      memory.entities.findOrCreate(client, "topic", "горы", SCOPE),
    );
    await initiative.record(entity.id, SCOPE);
    expect(await initiative.isSuppressed(entity.id, SCOPE)).toBe(true);

    // прямой вопрос всё равно находит память — suppression вне retrieval
    const found = await memory.retrieve(SCOPE, { textQuery: "горы" });
    expect(found.map((row) => row.text)).toContain("Пользователь любит горы");
  });
});


afterAll(async () => {
  await pool.end();
});
