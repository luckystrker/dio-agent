import type { PoolClient } from "pg";

// Entities (§8.1): идентификаторы предметов памяти. Описания и оценки
// живут записями памяти, не полями карточки. Scope у имени свой own.

export type EntityKind = "person" | "topic" | "place" | "object" | "event" | "project";

export const ENTITY_KINDS: readonly EntityKind[] = [
  "person",
  "topic",
  "place",
  "object",
  "event",
  "project",
];

export interface EntityRow {
  id: string;
  kind: EntityKind;
  canonicalName: string;
  scopeId: string;
}

interface EntityDbRow {
  id: string | bigint;
  kind: string;
  canonical_name: string;
  scope_id: string;
}

function toEntity(row: EntityDbRow): EntityRow {
  return {
    id: String(row.id),
    kind: row.kind as EntityKind,
    canonicalName: row.canonical_name,
    scopeId: row.scope_id,
  };
}

export class EntityRepo {
  /** Найти или создать сущность по имени в scope. Регистр имени нечувствителен. */
  async findOrCreate(
    client: PoolClient,
    kind: EntityKind,
    name: string,
    scopeId: string,
  ): Promise<EntityRow> {
    const lower = name.trim().toLowerCase();
    const existing = await client.query<EntityDbRow>(
      `SELECT id, kind, canonical_name, scope_id FROM entities
       WHERE scope_id = $1 AND lower(canonical_name) = $2
       LIMIT 1`,
      [scopeId, lower],
    );
    const found = existing.rows[0];
    if (found) return toEntity(found);

    const inserted = await client.query<EntityDbRow>(
      `INSERT INTO entities (kind, canonical_name, scope_id)
       VALUES ($1, $2, $3)
       ON CONFLICT DO NOTHING
       RETURNING id, kind, canonical_name, scope_id`,
      [kind, name.trim(), scopeId],
    );
    if (inserted.rows[0]) return toEntity(inserted.rows[0]);

    // гонка с параллельной вставкой: перечитываем
    const reread = await client.query<EntityDbRow>(
      `SELECT id, kind, canonical_name, scope_id FROM entities
       WHERE scope_id = $1 AND lower(canonical_name) = $2 LIMIT 1`,
      [scopeId, lower],
    );
    return toEntity(reread.rows[0]!);
  }

  /**
   * Поиск сущностей по имени/алиасу для аудитории (§8.1, §9.2):
   * видимы своя scope-зона и public. Приватная сущность не раскрывается
   * через поиск названий из чужого чата.
   */
  async resolveByName(
    client: PoolClient,
    name: string,
    audienceScope: string,
  ): Promise<EntityRow[]> {
    const lower = name.trim().toLowerCase();
    const result = await client.query<EntityDbRow>(
      `SELECT e.id, e.kind, e.canonical_name, e.scope_id
       FROM entities e
       LEFT JOIN entity_aliases a ON a.entity_id = e.id
       WHERE (lower(e.canonical_name) = $1 OR lower(a.alias) = $1)
         AND e.scope_id IN ($2, 'public')
       LIMIT 10`,
      [lower, audienceScope],
    );
    return result.rows.map(toEntity);
  }

  async addAlias(client: PoolClient, entityId: string, alias: string): Promise<void> {
    const entity = await client.query<{ scope_id: string }>(
      "SELECT scope_id FROM entities WHERE id = $1",
      [entityId],
    );
    const scopeId = entity.rows[0]?.["scope_id"];
    if (!scopeId) throw new Error(`сущность ${entityId} не найдена`);
    await client.query(
      `INSERT INTO entity_aliases (entity_id, alias, scope_id)
       VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
      [entityId, alias.trim(), scopeId],
    );
  }
}
