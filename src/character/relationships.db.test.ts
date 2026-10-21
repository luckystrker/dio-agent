// Интеграционный тест (живой PostgreSQL, база dio_test).
// §7.3: RelationshipRepo — familiarity растёт по runtime-правилу и капится
// на 1; affinity/trust меняются ограниченно (сжатие до ±0.1 за ход,
// границы [-1,1]/[0,1]); отсутствие записи — нейтральный дефолт.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { RelationshipRepo, RELATIONSHIP_LIMITS } from "./relationships.js";
import { MemoryRepo } from "../memory/memory-repo.js";
import { withTransaction } from "../runtime/transactions.js";
import { chatScope } from "../memory/scope.js";
import { cleanMemoryTables, createTestPool } from "../test-support/db.js";

let pool: Pool;
let repo: RelationshipRepo;
let memory: MemoryRepo;
let nextEntitySeq = 1;

const SCOPE = chatScope("700");

beforeEach(async () => {
  pool = createTestPool();
  repo = new RelationshipRepo(pool);
  memory = new MemoryRepo(pool);
  // relationships ссылается на entities: чистим связи, затем память (каскад)
  await pool.query("DELETE FROM relationships");
  await cleanMemoryTables(pool);
  nextEntitySeq = 1;
});

afterEach(async () => {
  await pool.query("DELETE FROM relationships").catch(() => undefined);
  await cleanMemoryTables(pool).catch(() => undefined);
  await pool.end();
});

/** Сущность-человек в scope чата — как создал бы resolveActor обработчика. */
async function makePerson(name = `человек ${nextEntitySeq++}`): Promise<string> {
  const entity = await withTransaction(pool, (client) =>
    memory.entities.findOrCreate(client, "person", name, SCOPE),
  );
  return entity.id;
}

describe("RelationshipRepo.read (§7.3)", () => {
  it("отсутствующая запись — нейтральный дефолт: familiarity 0, affinity 0, trust 0.2", async () => {
    const person = await makePerson();

    const relationship = await repo.read(person, SCOPE);

    expect(relationship).toEqual({
      personEntityId: person,
      scopeId: SCOPE,
      familiarity: 0,
      affinity: 0,
      trust: 0.2,
      updatedAt: expect.any(String) as string,
    });
    // строки в БД при этом не появилось — дефолт виртуальный
    const rows = await pool.query("SELECT 1 FROM relationships");
    expect(rows.rows).toEqual([]);
  });
});

describe("RelationshipRepo.applyDelta (§7.3: ограниченные изменения за ход)", () => {
  it("дельта +0.5 сжимается до ровно +0.1 по каждому полю", async () => {
    const person = await makePerson();

    const updated = await withTransaction(pool, (client) =>
      repo.applyDelta(client, person, SCOPE, { affinity: 0.5, trust: 0.5 }),
    );

    expect(updated.affinity).toBeCloseTo(0.1, 10); // §7.3: ровно лимит на ход, захардкожен
    expect(updated.trust).toBeCloseTo(0.2 + RELATIONSHIP_LIMITS.trustPerTurn, 10);
    expect(updated.familiarity).toBe(0); // дельта хода familiarity не трогает
  });

  it("отрицательная дельта -0.5 сжимается до -0.1", async () => {
    const person = await makePerson();

    const updated = await withTransaction(pool, (client) =>
      repo.applyDelta(client, person, SCOPE, { affinity: -0.5, trust: -0.5 }),
    );

    expect(updated.affinity).toBeCloseTo(-0.1, 10);
    expect(updated.trust).toBeCloseTo(0.2 - 0.1, 10); // 0.1, не -0.3
  });

  it("повторное применение в следующем ходе: накопление по 0.1, ensure не сбрасывает прогресс", async () => {
    const person = await makePerson();

    await withTransaction(pool, (client) =>
      repo.applyDelta(client, person, SCOPE, { affinity: 0.5 }),
    );
    const second = await withTransaction(pool, (client) =>
      repo.applyDelta(client, person, SCOPE, { affinity: 0.5 }),
    );

    // ON CONFLICT DO NOTHING в ensure не перезаписал уже накопленное
    expect(second.affinity).toBeCloseTo(0.2, 10);
  });

  it("границы: affinity зажат в [-1, 1], trust — в [0, 1]", async () => {
    const person = await makePerson();
    await withTransaction(pool, (client) =>
      client.query(
        "INSERT INTO relationships (person_entity_id, scope_id, familiarity, affinity, trust) VALUES ($1, $2, 0, 0.95, 0.05)",
        [person, SCOPE],
      ),
    );

    const up = await withTransaction(pool, (client) =>
      repo.applyDelta(client, person, SCOPE, { affinity: 0.1, trust: 0.1 }),
    );
    expect(up.affinity).toBeCloseTo(1, 10); // не 1.05
    expect(up.trust).toBeCloseTo(0.15, 10);

    await pool.query(
      "UPDATE relationships SET affinity = -0.95, trust = 0.05 WHERE person_entity_id = $1",
      [person],
    );
    const down = await withTransaction(pool, (client) =>
      repo.applyDelta(client, person, SCOPE, { affinity: -0.1, trust: -0.1 }),
    );
    expect(down.affinity).toBeCloseTo(-1, 10); // не -1.05
    expect(down.trust).toBeCloseTo(0, 10); // не -0.05
  });

  it("частичная дельта (только affinity) не трогает trust", async () => {
    const person = await makePerson();

    const updated = await withTransaction(pool, (client) =>
      repo.applyDelta(client, person, SCOPE, { affinity: 0.5 }),
    );

    expect(updated.affinity).toBeCloseTo(0.1, 10);
    expect(updated.trust).toBeCloseTo(0.2, 10);
  });
});

describe("RelationshipRepo.growFamiliarity (§7.3: runtime-правило значимых ходов)", () => {
  it("значимый ход: +0.05 к familiarity, affinity/trust не трогаются", async () => {
    const person = await makePerson();

    const updated = await withTransaction(pool, (client) =>
      repo.growFamiliarity(client, person, SCOPE),
    );

    expect(updated.familiarity).toBeCloseTo(RELATIONSHIP_LIMITS.familiarityPerSignificantTurn, 10);
    expect(updated.affinity).toBe(0);
    expect(updated.trust).toBeCloseTo(0.2, 10);
  });

  it("familiarity капится на 1 и не растёт дальше", async () => {
    const person = await makePerson();
    await withTransaction(pool, (client) =>
      client.query(
        "INSERT INTO relationships (person_entity_id, scope_id, familiarity, affinity, trust) VALUES ($1, $2, 0.98, 0, 0.2)",
        [person, SCOPE],
      ),
    );

    const capped = await withTransaction(pool, (client) =>
      repo.growFamiliarity(client, person, SCOPE),
    );
    expect(capped.familiarity).toBeCloseTo(1, 10); // 0.98 + 0.05 → 1, не 1.03

    const stillOne = await withTransaction(pool, (client) =>
      repo.growFamiliarity(client, person, SCOPE),
    );
    expect(stillOne.familiarity).toBeCloseTo(1, 10);
  });

  it("familiarity не откатывается decay-ом настроения и живёт своей строкой", async () => {
    // §7.3: отношения не сбрасываются затуханием mood — проверяем, что
    // familiarity переживает любые операции с mood_state (её носит
    // отдельная таблица с собственным updated_at)
    const person = await makePerson();
    await withTransaction(pool, (client) => repo.growFamiliarity(client, person, SCOPE));
    await pool.query("UPDATE mood_state SET valence = 0.9, updated_at = now() - interval '5 hours'").catch(() => undefined);

    const after = await repo.read(person, SCOPE);
    expect(after.familiarity).toBeCloseTo(0.05, 10);
  });
});
