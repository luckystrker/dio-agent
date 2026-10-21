// Интеграционный тест (живой PostgreSQL, база dio_test): репозиторий памяти.
// Г-критерии этапа 3: scope-изоляция (приватные записи/сущности/алиасы не
// всплывают из чужого чата ни через textQuery, ни через recent, ни через
// entityIds), supersede-цепочки (тихая перезапись запрещена), provenance
// (источник назначает runtime; FK не пускает несуществующий source_id).

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { MemoryRepo, type SourceRef } from "./memory-repo.js";
import type { MemoryCandidate } from "./memory-candidates.js";
import { chatScope } from "./scope.js";
import { withTransaction } from "../runtime/transactions.js";
import { cleanMemoryTables, createTestPool } from "../test-support/db.js";

let pool: Pool;
let memory: MemoryRepo;
let sourceCounter = 0;

beforeEach(async () => {
  pool = createTestPool();
  memory = new MemoryRepo(pool);
  await cleanMemoryTables(pool);
  sourceCounter = 0;
});

afterEach(async () => {
  // не оставляем записи следующим db-файлам: порядок файлов не гарантирован
  await cleanMemoryTables(pool);
  await pool.end();
});

/** Уникальный источник на каждую вставку — как разные ходы. */
function nextSource(): SourceRef {
  sourceCounter += 1;
  return { kind: "journal_event", ref: `turn:${sourceCounter}` };
}

async function insert(scopeId: string, candidate: MemoryCandidate): Promise<{ id: string }> {
  const source = nextSource();
  return withTransaction(pool, (client) => memory.insertCandidate(client, candidate, scopeId, source));
}

async function supersede(
  oldItemId: string,
  candidate: MemoryCandidate,
  scopeId: string,
): Promise<{ id: string }> {
  const source = nextSource();
  return withTransaction(pool, (client) =>
    memory.supersede(client, oldItemId, candidate, scopeId, source),
  );
}

const A = chatScope("111");
const B = chatScope("222");

interface ItemRow {
  id: string;
  status: string;
  scope_id: string;
  supersedes_id: string | null;
}

async function itemById(id: string): Promise<ItemRow | undefined> {
  const result = await pool.query<ItemRow>(
    "SELECT id::text, status, scope_id, supersedes_id::text FROM memory_items WHERE id = $1",
    [id],
  );
  return result.rows[0];
}

async function textOf(id: string): Promise<string> {
  const result = await pool.query<{ text: string }>("SELECT text FROM memory_items WHERE id = $1", [id]);
  return result.rows[0]!.text;
}

async function entityIdByName(lowerName: string): Promise<string> {
  const result = await pool.query<{ id: string }>(
    "SELECT id::text FROM entities WHERE lower(canonical_name) = $1",
    [lowerName],
  );
  return result.rows[0]!.id;
}

/** resolveByName требует клиента транзакции — обёртка для читающих проверок. */
async function resolveByName(name: string, audienceScope: string) {
  return withTransaction(pool, (client) =>
    memory.entities.resolveByName(client, name, audienceScope),
  );
}

describe("scope-изоляция (§9.1–9.2, Г-критерий)", () => {
  const PRIVATE_TEXT = "Алиса держит тайный пароль от сейфа в шкатулке";
  const PUBLIC_TEXT = "Публичный тайный пароль опубликован в открытом блоге";
  const QUERY = "тайный пароль";

  beforeEach(async () => {
    await insert(A, { kind: "claim", text: PRIVATE_TEXT, entities: [], salience: 0.9 });
    await insert("public", { kind: "claim", text: PUBLIC_TEXT, entities: [], salience: 0.5 });
  });

  it("textQuery: приватная запись чата A не находится из чата B; public видна обоим", async () => {
    const fromA = await memory.retrieve(A, { textQuery: QUERY });
    const fromB = await memory.retrieve(B, { textQuery: QUERY });

    expect(fromA.map((row) => row.text).sort()).toEqual([PRIVATE_TEXT, PUBLIC_TEXT].sort());
    expect(fromB.map((row) => row.text)).toEqual([PUBLIC_TEXT]);
  });

  it("recent: приватная запись не всплывает в недавних чужого чата", async () => {
    const fromB = await memory.retrieve(B);
    const fromA = await memory.retrieve(A);
    expect(fromB.map((row) => row.text)).toEqual([PUBLIC_TEXT]);
    expect(fromA.map((row) => row.text)).toContain(PRIVATE_TEXT);
  });

  it("entityIds: ссылка на сущность чужого чата не раскрывает запись (scope-фильтр в JOIN-ветке)", async () => {
    const item = await insert(A, {
      kind: "claim",
      text: "Секретный проект метеор запускается в марте",
      entities: [{ name: "Метеор", kind: "project", role: "subject" }],
    });
    const entityId = await entityIdByName("метеор");

    const fromB = await memory.retrieve(B, { entityIds: [entityId] });
    const fromA = await memory.retrieve(A, { entityIds: [entityId] });
    expect(fromB).toEqual([]);
    expect(fromA.map((row) => row.id)).toEqual([item.id]);
  });

  it("internal-запись не видна ни одному чату", async () => {
    await insert("internal", {
      kind: "thought",
      text: "Тайный пароль размышление внутреннее",
      entities: [],
    });
    const fromA = await memory.retrieve(A, { textQuery: QUERY });
    const fromB = await memory.retrieve(B, { textQuery: QUERY });
    expect(fromA.map((row) => row.text).sort()).toEqual([PRIVATE_TEXT, PUBLIC_TEXT].sort());
    expect(fromB.map((row) => row.text)).toEqual([PUBLIC_TEXT]);
  });
});

describe("изоляция сущностей и алиасов (§8.1, §9.2)", () => {
  it("сущность чата A не находится resolveByName из чата B; своя и public — находятся", async () => {
    await insert(A, {
      kind: "claim",
      text: "Секретный проект метеор запускается",
      entities: [{ name: "Метеор", kind: "project", role: "subject" }],
    });
    await pool.query(
      "INSERT INTO entities (kind, canonical_name, scope_id) VALUES ('topic', 'Новый год', 'public')",
    );

    expect(await resolveByName("метеор", A)).toHaveLength(1);
    expect(await resolveByName("метеор", B)).toEqual([]);
    // public-сущность видна из чужого чата, регистр имени не важен
    expect(await resolveByName("НОВЫЙ ГОД", B)).toHaveLength(1);
  });

  it("алиас приватной сущности не протекает в чужой чат", async () => {
    await insert(A, {
      kind: "claim",
      text: "Метеор стартует в марте",
      entities: [{ name: "Метеор", kind: "project", role: "subject" }],
    });
    const entityId = await entityIdByName("метеор");

    await withTransaction(pool, (client) => memory.entities.addAlias(client, entityId, "проект-икс"));

    const aliasRow = await pool.query<{ scope_id: string }>(
      "SELECT scope_id FROM entity_aliases WHERE alias = 'проект-икс'",
    );
    expect(aliasRow.rows[0]!.scope_id).toBe(A);

    expect(await resolveByName("проект-икс", A)).toHaveLength(1);
    expect(await resolveByName("проект-икс", B)).toEqual([]);
  });

  it("одно имя в двух чатах — две разные сущности, доступ не смешивается", async () => {
    await insert(A, {
      kind: "claim",
      text: "Горы любимы",
      entities: [{ name: "Горы", kind: "topic", role: "subject" }],
    });
    await insert(B, {
      kind: "claim",
      text: "Горы обсуждены",
      entities: [{ name: "Горы", kind: "topic", role: "subject" }],
    });

    const rows = await pool.query<{ id: string; scope_id: string }>(
      "SELECT id::text, scope_id FROM entities WHERE lower(canonical_name) = 'горы' ORDER BY id",
    );
    expect(rows.rows).toHaveLength(2);
    expect(new Set(rows.rows.map((row) => row.scope_id))).toEqual(new Set([A, B]));
  });
});

describe("supersede-цепочки (§10.3, Г-критерий: тихая перезапись запрещена)", () => {
  it("supersede помечает старую superseded, новая active с supersedes_id; история не стирается", async () => {
    const old = await insert(A, { kind: "claim", text: "Пользователь живёт в Москве", entities: [] });
    const updated = await supersede(old.id, { kind: "claim", text: "Пользователь живёт в Питере", entities: [] }, A);

    const oldRow = await itemById(old.id);
    const newRow = await itemById(updated.id);
    expect(oldRow!.status).toBe("superseded");
    expect(oldRow!.scope_id).toBe(A);
    expect(newRow!.status).toBe("active");
    expect(newRow!.supersedes_id).toBe(old.id);

    // старый текст физически на месте — перезаписи нет (§10.3)
    expect(await textOf(old.id)).toBe("Пользователь живёт в Москве");

    // основание производности зафиксировано (derived ← input)
    const derivation = await pool.query(
      "SELECT 1 FROM memory_derivations WHERE derived_id = $1 AND input_memory_id = $2",
      [updated.id, old.id],
    );
    expect(derivation.rows).toHaveLength(1);

    // у новой записи свой назначенный runtime-ом источник (§10.3: отдельный источник)
    const newSource = await pool.query<{ ref: string }>(
      "SELECT s.ref FROM memory_sources ms JOIN sources s ON s.id = ms.source_id WHERE ms.memory_id = $1",
      [updated.id],
    );
    expect(newSource.rows.map((row) => row.ref)).toEqual([`turn:2`]);
  });

  it("цепочка A ← B ← C: supersedeChain идёт к корню, история преемственности целая", async () => {
    const a = await insert(A, { kind: "claim", text: "живу в Москве", entities: [] });
    const b = await supersede(a.id, { kind: "claim", text: "живу в Питере", entities: [] }, A);
    const c = await supersede(b.id, { kind: "claim", text: "живу в Казани", entities: [] }, A);

    const fromLeaf = await memory.supersedeChain(c.id, A);
    expect(fromLeaf.map((row) => row.id)).toEqual([c.id, b.id, a.id]);
    expect(fromLeaf.map((row) => row.text)).toEqual(["живу в Казани", "живу в Питере", "живу в Москве"]);

    const fromMiddle = await memory.supersedeChain(b.id, A);
    expect(fromMiddle.map((row) => row.id)).toEqual([b.id, a.id]);
  });

  it("superseded не выдаётся retrieve; активна только последняя версия", async () => {
    const a = await insert(A, { kind: "claim", text: "живу в Москве", entities: [] });
    const b = await supersede(a.id, { kind: "claim", text: "живу в Питере", entities: [] }, A);

    const byText = await memory.retrieve(A, { textQuery: "живу" });
    const recent = await memory.retrieve(A);
    expect(byText.map((row) => row.id)).toEqual([b.id]);
    expect(recent.map((row) => row.id)).toEqual([b.id]);
    // но цепочка по-прежнему достаёт историю (преемственность не стёрта)
    expect(await memory.supersedeChain(b.id, A)).toHaveLength(2);
  });

  it("supersede со сменой scope отклоняется: перенос приватной записи в чужой чат запрещён (§9.1)", async () => {
    const a = await insert(A, { kind: "claim", text: "приватное мнение чата A", entities: [] });

    await expect(
      supersede(a.id, { kind: "claim", text: "попытка легализовать в чате B", entities: [] }, B),
    ).rejects.toThrow(/смена scope запрещена/);

    // запись не изменилась, вторая (форк/копия) не создана, в чат B ничего не протекло
    expect((await itemById(a.id))!.status).toBe("active");
    expect((await itemById(a.id))!.scope_id).toBe(A);
    const total = await pool.query<{ count: string }>("SELECT count(*)::text AS count FROM memory_items");
    expect(total.rows[0]!.count).toBe("1");
    expect(await memory.retrieve(B)).toEqual([]);
  });

  it("supersedeChain фильтрует по аудитории: чужой scope отсечён, свой и public на месте (§9.2)", async () => {
    // Цепочка public-корень ← chat:B ← chat:A-лист. Сквозное звено чужого
    // scope supersede создать не может (guard выше), такая цепочка возможна
    // только обслуживающей операцией — audience-фильтр обязан держать и её.
    const root = await insert("public", { kind: "claim", text: "публичная версия мнения", entities: [] });
    const foreign = await pool.query<{ id: string }>(
      `INSERT INTO memory_items (kind, text, scope_id, status)
       VALUES ('claim', 'чужая приватная версия', $1, 'superseded') RETURNING id::text`,
      [B],
    );
    const foreignId = foreign.rows[0]!.id;
    const leaf = await insert(A, { kind: "claim", text: "своя свежая версия", entities: [] });
    await pool.query("UPDATE memory_items SET supersedes_id = $2 WHERE id = $1", [foreignId, root.id]);
    await pool.query("UPDATE memory_items SET supersedes_id = $2 WHERE id = $1", [leaf.id, foreignId]);
    // детерминированный порядок цепочки (ORDER BY recorded_at DESC)
    const stamps: Array<[string, string]> = [
      [root.id, "2026-10-20T10:00:00Z"],
      [foreignId, "2026-10-20T11:00:00Z"],
      [leaf.id, "2026-10-20T12:00:00Z"],
    ];
    for (const [id, stamp] of stamps) {
      await pool.query("UPDATE memory_items SET recorded_at = $2 WHERE id = $1", [id, stamp]);
    }

    // из чата A: chat:B-звено отсечено, лист и public-корень на месте
    const fromA = await memory.supersedeChain(leaf.id, A);
    expect(fromA.map((row) => row.id)).toEqual([leaf.id, root.id]);
    expect(fromA.map((row) => row.text)).toEqual(["своя свежая версия", "публичная версия мнения"]);

    // из чата B: наоборот — отсечён chat:A-лист, своё звено и public видны
    const fromB = await memory.supersedeChain(leaf.id, B);
    expect(fromB.map((row) => row.id)).toEqual([foreignId, root.id]);
    expect(fromB.map((row) => row.scopeId)).toEqual([B, "public"]);
  });

  it("повторный supersede уже superseded-записи отклоняется: форк текущей версии запрещён (§10.3)", async () => {
    // После фикса: заменять можно только active/disputed — повторный supersede
    // той же записи отклоняется, единственность текущей версии гарантирована.
    const a = await insert(A, { kind: "claim", text: "живу в Москве", entities: [] });
    const b = await supersede(a.id, { kind: "claim", text: "живу в Питере", entities: [] }, A);

    await expect(
      supersede(a.id, { kind: "claim", text: "живу в Казани", entities: [] }, A),
    ).rejects.toThrow(/форк запрещён/);

    // состояние неизменно: одна активная версия
    expect((await itemById(a.id))!.status).toBe("superseded");
    expect((await itemById(b.id))!.status).toBe("active");
    const active = await memory.retrieve(A, { textQuery: "живу" });
    expect(active.map((row) => row.id)).toEqual([b.id]);
  });

  it("ensureSource идемпотентен: (kind, ref) → один и тот же id, одна строка", async () => {
    let first = "";
    let second = "";
    let other = "";
    await withTransaction(pool, async (client) => {
      first = await memory.ensureSource(client, { kind: "journal_event", ref: "turn:42" });
      second = await memory.ensureSource(client, { kind: "journal_event", ref: "turn:42" });
      other = await memory.ensureSource(client, { kind: "journal_event", ref: "turn:43" });
    });
    expect(second).toBe(first);
    expect(other).not.toBe(first);
    const rows = await pool.query<{ ref: string }>(
      "SELECT ref FROM sources WHERE kind = 'journal_event' ORDER BY ref",
    );
    expect(rows.rows.map((row) => row.ref)).toEqual(["turn:42", "turn:43"]);
  });

  it("insertCandidate назначает источник сам: memory_items ↔ sources связаны", async () => {
    const item = await insert(A, { kind: "experience", text: "обсуждение", entities: [] });
    const rows = await pool.query<{ kind: string; ref: string }>(
      "SELECT s.kind, s.ref FROM memory_sources ms JOIN sources s ON s.id = ms.source_id WHERE ms.memory_id = $1",
      [item.id],
    );
    expect(rows.rows).toHaveLength(1);
    expect(rows.rows[0]!.kind).toBe("journal_event");
    expect(rows.rows[0]!.ref).toBe("turn:1");
  });

  it("FK не позволяет сослаться на несуществующий источник: source_id=999999 → 23503", async () => {
    const item = await pool.query<{ id: string }>(
      "INSERT INTO memory_items (kind, text, scope_id) VALUES ('claim', 'текст', $1) RETURNING id::text",
      [A],
    );
    await expect(
      pool.query("INSERT INTO memory_sources (memory_id, source_id) VALUES ($1, 999999)", [
        item.rows[0]!.id,
      ]),
    ).rejects.toMatchObject({ code: "23503" });
  });
});

describe("retrieval (§10.4)", () => {
  it("full-text находит по русскому тексту в другой словоформе (websearch_to_tsquery russian)", async () => {
    await insert(A, {
      kind: "experience",
      text: "Мы обсуждали восхождение в горах Памира",
      entities: [],
    });
    await insert(A, { kind: "experience", text: "Рецепт сырников из творога", entities: [] });

    const found = await memory.retrieve(A, { textQuery: "восхождения горах" });
    expect(found.map((row) => row.text)).toEqual(["Мы обсуждали восхождение в горах Памира"]);

    const miss = await memory.retrieve(A, { textQuery: "несуществующееслово" });
    expect(miss).toEqual([]);
  });

  it("порядок salience DESC доминирует над свежестью; limit режет сверху", async () => {
    // самая старая запись — самая значимая: salience обязан победить recorded_at
    const low = await insert(A, { kind: "claim", text: "кофе сорта арабика", entities: [], salience: 0.5 });
    const mid = await insert(A, { kind: "claim", text: "кофе сорта робуста", entities: [], salience: 0.7 });
    const top = await insert(A, { kind: "claim", text: "кофе напиток утра", entities: [], salience: 0.9 });
    const stamps = new Map<string, string>([
      [low.id, "2026-10-20T10:00:00Z"],
      [mid.id, "2026-10-20T11:00:00Z"],
      [top.id, "2026-10-20T09:00:00Z"],
    ]);
    for (const [id, stamp] of stamps) {
      await pool.query("UPDATE memory_items SET recorded_at = $2 WHERE id = $1", [id, stamp]);
    }

    const all = await memory.retrieve(A, { textQuery: "кофе" });
    expect(all.map((row) => row.id)).toEqual([top.id, mid.id, low.id]);

    const limited = await memory.retrieve(A, { textQuery: "кофе", limit: 2 });
    expect(limited.map((row) => row.id)).toEqual([top.id, mid.id]);
  });

  it("entityIds-ветка: только записи, связанные с запрошенной сущностью", async () => {
    const withX = await insert(A, {
      kind: "claim",
      text: "запись про икс",
      entities: [{ name: "икс", kind: "topic", role: "subject" }],
    });
    await insert(A, {
      kind: "claim",
      text: "запись про игрек",
      entities: [{ name: "игрек", kind: "topic", role: "subject" }],
    });
    const x = await entityIdByName("икс");
    const found = await memory.retrieve(A, { entityIds: [x] });
    expect(found.map((row) => row.id)).toEqual([withX.id]);
  });

  it("recent-ветка: новые раньше старых, дефолтный limit 10", async () => {
    for (let i = 1; i <= 12; i += 1) {
      const item = await insert(A, { kind: "experience", text: `событие номер ${i}`, entities: [] });
      await pool.query("UPDATE memory_items SET recorded_at = $2 WHERE id = $1", [
        item.id,
        new Date(Date.UTC(2026, 9, 1, 0, i)).toISOString(),
      ]);
    }
    const recent = await memory.retrieve(A);
    expect(recent).toHaveLength(10);
    expect(recent[0]!.text).toBe("событие номер 12");
    expect(recent[9]!.text).toBe("событие номер 3");
  });

  it("public+scope: из одного чата видны обе зоны, из чужого — только public", async () => {
    await insert(A, { kind: "claim", text: "приватный факт про памир", entities: [] });
    await insert("public", { kind: "claim", text: "публичный факт про памир", entities: [] });

    const fromA = await memory.retrieve(A, { textQuery: "памир" });
    const fromB = await memory.retrieve(B, { textQuery: "памир" });
    expect(fromA.map((row) => row.scopeId).sort()).toEqual([A, "public"].sort());
    expect(fromB.map((row) => row.scopeId)).toEqual(["public"]);
  });
});

describe("stances для сборки контекста (§8.4, §10.4 шаг 3: актуальное отношение по сущностям)", () => {
  async function seedEntity(name: string, scopeId: string): Promise<string> {
    const result = await pool.query<{ id: string }>(
      "INSERT INTO entities (kind, canonical_name, scope_id) VALUES ('topic', $1, $2) RETURNING id::text",
      [name, scopeId],
    );
    return result.rows[0]!.id;
  }

  /** Stances пишет фоновая консолидация — вставляем напрямую, как она. */
  async function seedStance(
    entityId: string,
    aspect: string,
    scopeId: string,
    summary: string,
    strength: number,
    interest: number,
  ): Promise<void> {
    await pool.query(
      "INSERT INTO stances (entity_id, aspect, scope_id, summary, strength, interest) VALUES ($1, $2, $3, $4, $5, $6)",
      [entityId, aspect, scopeId, summary, strength, interest],
    );
  }

  it("видны stances своего scope и public; чужого чата и internal — нет (§9.2)", async () => {
    const own = await seedEntity("горы", A);
    const pub = await seedEntity("праздник", "public");
    const foreign = await seedEntity("море", B);
    const internal = await seedEntity("самоанализ", "internal");
    await seedStance(own, "atmosphere", A, "люблю тишину гор", 0.8, 0.6);
    await seedStance(pub, "general", "public", "смешанное отношение к суете", 0.5, 0.4);
    await seedStance(foreign, "general", B, "чужое приватное мнение о море", 0.9, 0.9);
    await seedStance(internal, "general", "internal", "внутренняя заметка персоны", 0.7, 0.2);

    const visible = await memory.stancesForEntities(A, [own, pub, foreign, internal]);

    expect(visible).toHaveLength(2);
    expect(visible.map((stance) => stance.entityId).sort()).toEqual([own, pub].sort());
    expect(visible.find((stance) => stance.entityId === own)).toMatchObject({
      aspect: "atmosphere",
      summary: "люблю тишину гор",
      strength: 0.8,
      interest: 0.6,
    });
    expect(visible.find((stance) => stance.entityId === pub)).toMatchObject({
      aspect: "general",
      summary: "смешанное отношение к суете",
      strength: 0.5,
      interest: 0.4,
    });
    // чужие не протекают ни текстом, ни оценками
    const summaries = visible.map((stance) => stance.summary);
    expect(summaries).not.toContain("чужое приватное мнение о море");
    expect(summaries).not.toContain("внутренняя заметка персоны");
  });

  it("фильтр по scope самого stance: приватный stance на public-сущность не виден чужому чату", async () => {
    // одна public-сущность, stance на неё в двух чатах — пересечения быть не может
    const entity = await seedEntity("новый год", "public");
    await seedStance(entity, "atmosphere", A, "нравится уют дома", 0.7, 0.5);
    await seedStance(entity, "atmosphere", B, "не нравится шум", 0.6, 0.4);

    const fromA = await memory.stancesForEntities(A, [entity]);
    expect(fromA).toHaveLength(1);
    expect(fromA[0]!.summary).toBe("нравится уют дома");

    const fromB = await memory.stancesForEntities(B, [entity]);
    expect(fromB).toHaveLength(1);
    expect(fromB[0]!.summary).toBe("не нравится шум");
  });

  it("пустой список сущностей — пустой результат без похода в БД", async () => {
    const entity = await seedEntity("горы", A);
    await seedStance(entity, "atmosphere", A, "люблю тишину гор", 0.8, 0.6);
    expect(await memory.stancesForEntities(A, [])).toEqual([]);
  });
});
