import type { Pool, PoolClient } from "pg";

// Отношения (§7.3): числовые регуляторы отношения к человеку по чату.
// Familiarity растёт по runtime-правилу от значимых взаимодействий;
// affinity/trust меняются ограниченно и с основанием (основание — заметкой
// в state_notes при применении хода). Не сбрасываются decay-ом настроения.

export interface Relationship {
  personEntityId: string;
  scopeId: string;
  familiarity: number; // 0..1
  affinity: number; // -1..1
  trust: number; // 0..1
  updatedAt: string;
}

/** Лимиты изменения за ход (§7.3). */
export const RELATIONSHIP_LIMITS = {
  affinityPerTurn: 0.1,
  trustPerTurn: 0.1,
  familiarityPerSignificantTurn: 0.05,
} as const;

const DEFAULT_TRUST = 0.2;

function clamp(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, value));
}

interface RelationshipDbRow {
  person_entity_id: string | bigint;
  scope_id: string;
  familiarity: string;
  affinity: string;
  trust: string;
  updated_at: Date;
}

function toRelationship(row: RelationshipDbRow): Relationship {
  return {
    personEntityId: String(row.person_entity_id),
    scopeId: row.scope_id,
    familiarity: Number(row.familiarity),
    affinity: Number(row.affinity),
    trust: Number(row.trust),
    updatedAt: row.updated_at.toISOString(),
  };
}

export class RelationshipRepo {
  constructor(private readonly pool: Pool) {}

  /** Читать отношение; отсутствие записи — нейтральное стартовое. */
  async read(personEntityId: string, scopeId: string): Promise<Relationship> {
    const result = await this.pool.query<RelationshipDbRow>(
      `SELECT person_entity_id, scope_id, familiarity, affinity, trust, updated_at
       FROM relationships WHERE person_entity_id = $1 AND scope_id = $2`,
      [personEntityId, scopeId],
    );
    const row = result.rows[0];
    return row
      ? toRelationship(row)
      : {
          personEntityId,
          scopeId,
          familiarity: 0,
          affinity: 0,
          trust: DEFAULT_TRUST,
          updatedAt: new Date().toISOString(),
        };
  }

  /** Создать строку, если ещё нет (внутри транзакции применения). */
  async ensure(client: PoolClient, personEntityId: string, scopeId: string): Promise<void> {
    await client.query(
      `INSERT INTO relationships (person_entity_id, scope_id, trust)
       VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
      [personEntityId, scopeId, DEFAULT_TRUST],
    );
  }

  /**
   * Применить ограниченную дельту affinity/trust за ход ВНУТРИ транзакции.
   * Изменение сжимается до ±PerTurn от текущего значения.
   */
  async applyDelta(
    client: PoolClient,
    personEntityId: string,
    scopeId: string,
    delta: { affinity?: number; trust?: number },
  ): Promise<Relationship> {
    await this.ensure(client, personEntityId, scopeId);
    const current = await client.query<RelationshipDbRow>(
      `SELECT person_entity_id, scope_id, familiarity, affinity, trust, updated_at
       FROM relationships WHERE person_entity_id = $1 AND scope_id = $2 FOR UPDATE`,
      [personEntityId, scopeId],
    );
    const row = current.rows[0]!;
    const nextAffinity = clamp(
      Number(row.affinity) + clamp(delta.affinity ?? 0, -RELATIONSHIP_LIMITS.affinityPerTurn, RELATIONSHIP_LIMITS.affinityPerTurn),
      -1,
      1,
    );
    const nextTrust = clamp(
      Number(row.trust) + clamp(delta.trust ?? 0, -RELATIONSHIP_LIMITS.trustPerTurn, RELATIONSHIP_LIMITS.trustPerTurn),
      0,
      1,
    );
    const updated = await client.query<RelationshipDbRow>(
      `UPDATE relationships SET affinity = $3, trust = $4, updated_at = now()
       WHERE person_entity_id = $1 AND scope_id = $2
       RETURNING person_entity_id, scope_id, familiarity, affinity, trust, updated_at`,
      [personEntityId, scopeId, nextAffinity, nextTrust],
    );
    return toRelationship(updated.rows[0]!);
  }

  /** Familiarity растёт от значимого взаимодействия (runtime-правило, не модель). */
  async growFamiliarity(
    client: PoolClient,
    personEntityId: string,
    scopeId: string,
    amount: number = RELATIONSHIP_LIMITS.familiarityPerSignificantTurn,
  ): Promise<Relationship> {
    await this.ensure(client, personEntityId, scopeId);
    const updated = await client.query<RelationshipDbRow>(
      `UPDATE relationships
       SET familiarity = LEAST(1, familiarity + $3), updated_at = now()
       WHERE person_entity_id = $1 AND scope_id = $2
       RETURNING person_entity_id, scope_id, familiarity, affinity, trust, updated_at`,
      [personEntityId, scopeId, amount],
    );
    return toRelationship(updated.rows[0]!);
  }
}

/** Детерминированное описание для контекста. */
export function describeRelationship(relationship: Relationship): string {
  const parts: string[] = [];
  if (relationship.familiarity < 0.2) parts.push("почти незнаком");
  else if (relationship.familiarity < 0.6) parts.push("уже неплохо знаком");
  else parts.push("хорошо знаешь");

  if (relationship.affinity > 0.3) parts.push("он тебе симпатичен");
  else if (relationship.affinity < -0.3) parts.push("он тебе неприятен");

  if (relationship.trust > 0.6) parts.push("доверяешь ему");
  else if (relationship.trust < 0.15) parts.push("доверия мало");

  return parts.join(", ");
}
