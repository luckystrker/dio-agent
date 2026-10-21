import type { Pool, PoolClient } from "pg";
import { EntityRepo, type EntityKind } from "./entities.js";
import type { MemoryCandidate, MemoryKind } from "./memory-candidates.js";
import type { ScopeId } from "./scope.js";

// Запись/извлечение памяти (§8, §10). Единственный пишущий путь — runtime:
// модель предлагает кандидатов, источник назначается здесь и только здесь.

export type SourceKind = "journal_event" | "web_document" | "runtime_action" | "persona_version";

export interface SourceRef {
  kind: SourceKind;
  ref: string;
  metadata?: Record<string, unknown>;
}

export interface MemoryItemRow {
  id: string;
  kind: MemoryKind;
  text: string;
  scopeId: string;
  status: string;
  salience: number;
  confidence: number | null;
  recordedAt: string;
}

interface MemoryDbRow {
  id: string | bigint;
  kind: string;
  text: string;
  scope_id: string;
  status: string;
  salience: string;
  confidence: string | null;
  recorded_at: Date;
}

function toRow(row: MemoryDbRow): MemoryItemRow {
  return {
    id: String(row.id),
    kind: row.kind as MemoryKind,
    text: row.text,
    scopeId: row.scope_id,
    status: row.status,
    salience: Number(row.salience),
    confidence: row.confidence === null ? null : Number(row.confidence),
    recordedAt: row.recorded_at.toISOString(),
  };
}

export class MemoryRepo {
  readonly entities = new EntityRepo();

  constructor(private readonly pool: Pool) {}

  /** Источник создаётся/переиспользуется по (kind, ref). Возвращает id. */
  async ensureSource(client: PoolClient, source: SourceRef): Promise<string> {
    await client.query(
      `INSERT INTO sources (kind, ref, metadata) VALUES ($1, $2, $3)
       ON CONFLICT (kind, ref) DO NOTHING`,
      [source.kind, source.ref, JSON.stringify(source.metadata ?? {})],
    );
    const found = await client.query<{ id: string }>(
      "SELECT id::text FROM sources WHERE kind = $1 AND ref = $2",
      [source.kind, source.ref],
    );
    return found.rows[0]!.id;
  }

  /**
   * Записать кандидата памяти от имени runtime: item + links + source.
   * Scope назначает runtime (scope хода), не модель.
   */
  async insertCandidate(
    client: PoolClient,
    candidate: MemoryCandidate,
    scopeId: ScopeId,
    source: SourceRef,
  ): Promise<MemoryItemRow> {
    const sourceId = await this.ensureSource(client, source);

    const inserted = await client.query<MemoryDbRow>(
      `INSERT INTO memory_items (kind, text, scope_id, salience, confidence)
       VALUES ($1, $2, $3, $4, $5)
       RETURNING id, kind, text, scope_id, status, salience, confidence, recorded_at`,
      [
        candidate.kind,
        candidate.text,
        scopeId,
        candidate.salience ?? 0.5,
        candidate.confidence ?? null,
      ],
    );
    const item = toRow(inserted.rows[0]!);

    await client.query(
      "INSERT INTO memory_sources (memory_id, source_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
      [item.id, sourceId],
    );

    for (const entity of candidate.entities ?? []) {
      const entityRow = await this.entities.findOrCreate(
        client,
        entity.kind as EntityKind,
        entity.name,
        scopeId,
      );
      await client.query(
        `INSERT INTO memory_links (memory_id, entity_id, role) VALUES ($1, $2, $3)
         ON CONFLICT DO NOTHING`,
        [item.id, entityRow.id, entity.role],
      );
    }
    return item;
  }

  /**
   * Supersede (§8.2, §10.3): новая запись заменяет старую; старая помечается
   * superseded, история преемственности сохраняется. Тихая перезапись
   * запрещена — заменять можно только активную (или оспоренную) запись,
   * иначе плодились бы «форки» текущей версии одного мнения.
   */
  async supersede(
    client: PoolClient,
    oldItemId: string,
    candidate: MemoryCandidate,
    scopeId: ScopeId,
    source: SourceRef,
  ): Promise<MemoryItemRow> {
    const oldItem = await client.query<{ scope_id: string; status: string }>(
      "SELECT scope_id, status FROM memory_items WHERE id = $1",
      [oldItemId],
    );
    const old = oldItem.rows[0];
    if (!old) throw new Error(`supersede: запись ${oldItemId} не найдена`);
    if (old.scope_id !== scopeId) {
      throw new Error("supersede: смена scope запрещена — пересечение аудиторий §9.1");
    }
    if (old.status !== "active" && old.status !== "disputed") {
      throw new Error(`supersede: запись ${oldItemId} уже ${old.status} — форк запрещён (§10.3)`);
    }

    const newItem = await this.insertCandidate(client, candidate, scopeId, source);
    await client.query(
      `UPDATE memory_items SET supersedes_id = $2 WHERE id = $1`,
      [newItem.id, oldItemId],
    );
    await client.query(
      `UPDATE memory_items SET status = 'superseded' WHERE id = $1 AND status = 'active'`,
      [oldItemId],
    );
    await client.query(
      `INSERT INTO memory_derivations (derived_id, input_memory_id) VALUES ($1, $2)
       ON CONFLICT DO NOTHING`,
      [newItem.id, oldItemId],
    );
    return newItem;
  }

  /** Цепочка supersede от записи к корню (преемственность мнений), §9.2: только записи, видимые аудитории. */
  async supersedeChain(itemId: string, audienceScope: ScopeId): Promise<MemoryItemRow[]> {
    const result = await this.pool.query<MemoryDbRow>(
      `WITH RECURSIVE chain AS (
         SELECT m.* FROM memory_items m WHERE m.id = $1::bigint
         UNION ALL
         SELECT prev.* FROM memory_items prev
         JOIN chain ON prev.id = chain.supersedes_id
       )
       SELECT id, kind, text, scope_id, status, salience, confidence, recorded_at
       FROM chain
       WHERE scope_id IN ($2, 'public')
       ORDER BY recorded_at DESC`,
      [itemId, audienceScope],
    );
    return result.rows.map(toRow);
  }

  /**
   * Retrieval (§10.4, MVP-срез): записи аудитории по прямым связям с
   * сущностями + недавние записи scope + full-text fallback. Порядок:
   * salience DESC, recorded_at DESC. Scope-фильтр до любого ранжирования (§9.2).
   */
  async retrieve(
    scopeId: ScopeId,
    options: { entityIds?: string[]; textQuery?: string; limit?: number } = {},
  ): Promise<MemoryItemRow[]> {
    const limit = options.limit ?? 10;
    // Видимость (§9.2): своя scope-зона и public; фильтр до ранжирования
    const visibility = "scope_id IN ($1, 'public')";
    const params: unknown[] = [scopeId];

    if (options.entityIds && options.entityIds.length > 0) {
      params.push(options.entityIds.map(String), limit);
      const result = await this.pool.query<MemoryDbRow>(
        `SELECT DISTINCT m.id, m.kind, m.text, m.scope_id, m.status, m.salience, m.confidence, m.recorded_at
         FROM memory_items m
         JOIN memory_links l ON l.memory_id = m.id
         WHERE m.status = 'active' AND m.${visibility}
           AND l.entity_id = ANY($2::bigint[])
         ORDER BY m.salience DESC, m.recorded_at DESC
         LIMIT $3`,
        params,
      );
      return result.rows.map(toRow);
    }

    if (options.textQuery && options.textQuery.trim().length > 0) {
      params.push(options.textQuery.trim(), limit);
      const result = await this.pool.query<MemoryDbRow>(
        `SELECT id, kind, text, scope_id, status, salience, confidence, recorded_at
         FROM memory_items
         WHERE status = 'active' AND ${visibility}
           AND to_tsvector('russian', text) @@ websearch_to_tsquery('russian', $2)
         ORDER BY salience DESC, recorded_at DESC
         LIMIT $3`,
        params,
      );
      return result.rows.map(toRow);
    }

    params.push(limit);
    const recent = await this.pool.query<MemoryDbRow>(
      `SELECT id, kind, text, scope_id, status, salience, confidence, recorded_at
       FROM memory_items
       WHERE status = 'active' AND ${visibility}
       ORDER BY recorded_at DESC
       LIMIT $2`,
      params,
    );
    return recent.rows.map(toRow);
  }

  /** Stance по сущности для аудитории (§8.4, §10.4 шаг 3). */
  async stancesForEntities(
    scopeId: ScopeId,
    entityIds: string[],
  ): Promise<Array<{ entityId: string; aspect: string; summary: string; strength: number; interest: number }>> {
    if (entityIds.length === 0) return [];
    const result = await this.pool.query<{
      entity_id: string;
      aspect: string;
      summary: string;
      strength: string;
      interest: string;
    }>(
      `SELECT entity_id::text, aspect, summary, strength, interest
       FROM stances
       WHERE scope_id IN ($1, 'public') AND entity_id = ANY($2::bigint[])`,
      [scopeId, entityIds.map(String)],
    );
    return result.rows.map((row) => ({
      entityId: row.entity_id,
      aspect: row.aspect,
      summary: row.summary,
      strength: Number(row.strength),
      interest: Number(row.interest),
    }));
  }
}
