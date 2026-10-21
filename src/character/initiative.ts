import type { Pool } from "pg";

// Подавление повторной инициативы (§2, инвариант «повторный recall — не
// новое доказательство»; §10.4: подавление применяется к ИНИЦИАТИВНЫМ
// упоминаниям, отдельно от retrieval). Прямой вопрос это ограничение
// не встречает никогда: retrieval не фильтруется этим механизмом.

export const INITIATIVE_SUPPRESSION_DAYS = 3;

export class InitiativeRepo {
  constructor(private readonly pool: Pool) {}

  /** Подавлена ли инициатива по теме (недавний собственный подъём темы). */
  async isSuppressed(entityId: string, scopeId: string): Promise<boolean> {
    const result = await this.pool.query<{ exists: boolean }>(
      `SELECT EXISTS (
         SELECT 1 FROM initiative_topics
         WHERE entity_id = $1 AND scope_id = $2
           AND last_initiative_at > now() - ($3::int * interval '1 day')
       ) AS exists`,
      [entityId, scopeId, INITIATIVE_SUPPRESSION_DAYS],
    );
    return result.rows[0]?.["exists"] === true;
  }

  /** Зафиксировать собственный подъём темы (в момент инициативы). */
  async record(entityId: string, scopeId: string): Promise<void> {
    await this.pool.query(
      `INSERT INTO initiative_topics (entity_id, scope_id, last_initiative_at)
       VALUES ($1, $2, now())
       ON CONFLICT (entity_id, scope_id) DO UPDATE SET last_initiative_at = now()`,
      [entityId, scopeId],
    );
  }
}
