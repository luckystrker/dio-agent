import { afterAll, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { PersonaRepo, DEFAULT_PERSONA } from "./persona.js";
import { createTestPool } from "../test-support/db.js";

// 4.1: версионированная persona; из пустой базы создаётся версия 1
// по умолчанию; повторный вызов читает существующую, не плодит версии.

const pool: Pool = await createTestPool();
const repo = new PersonaRepo(pool);

beforeEach(async () => {
  await pool.query("DELETE FROM personas");
});

describe("PersonaRepo.active (§7.1)", () => {
  it("пустая база: создаётся активная версия 1 с дефолтной Dio", async () => {
    const persona = await repo.active();
    expect(persona.version).toBe(1);
    expect(persona.name).toBe(DEFAULT_PERSONA.name);
    expect(persona.moodBaseline).toEqual({ ...DEFAULT_PERSONA.moodBaseline });
    expect(persona.seedBio).toContain("persona_seed");

    const rows = await pool.query<{ count: string }>("SELECT count(*)::text AS count FROM personas");
    expect(Number(rows.rows[0]!.count)).toBe(1);
  });

  it("повторный вызов читает существующую, не создаёт новую версию", async () => {
    await repo.active();
    const again = await repo.active();
    expect(again.version).toBe(1);
    const rows = await pool.query<{ count: string }>("SELECT count(*)::text AS count FROM personas");
    expect(Number(rows.rows[0]!.count)).toBe(1);
  });

  it("активна версия с наибольшим номером (явная смена persona)", async () => {
    await repo.active();
    await pool.query(
      `INSERT INTO personas (version, active, name, description, speech_style, values,
                             mood_valence, mood_arousal, mood_social_energy)
       VALUES (2, TRUE, 'Dio v2', 'обновлённый характер', 'иначе', '["честность"]'::jsonb, 0, 0.5, 0.5)`,
    );
    const persona = await repo.active();
    expect(persona.version).toBe(2);
    expect(persona.name).toBe("Dio v2");
  });
});

afterAll(async () => {
  await pool.end();
});
