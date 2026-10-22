import { afterAll, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { PersonaRepo, DEFAULT_PERSONA } from "./persona.js";
import { createTestPool } from "../test-support/db.js";

// 4.1: версионированная persona; из пустой базы создаётся версия 1
// по умолчанию; повторный вызов читает существующую, не плодит версии.

const pool: Pool = await createTestPool();
const repo = new PersonaRepo(pool);

const SOURCE = {
  name: "Dio",
  description: "описание из файла",
  speechStyle: "манера из файла",
  values: ["честность", "любопытство"],
  moodBaseline: { valence: 0.1, arousal: 0.4, socialEnergy: 0.6 },
  seedBio: "биография-вымысел из файла",
};

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

describe("PersonaRepo.ensureFromSource (§7.1: файл — источник истины)", () => {
  it("пустая база: источник из файла становится активной версией 1", async () => {
    const persona = await repo.ensureFromSource(SOURCE);
    expect(persona.version).toBe(1);
    expect(persona.name).toBe("Dio");
    expect(persona.description).toBe("описание из файла");
    expect(persona.seedBio).toBe("биография-вымысел из файла");
  });

  it("источник совпадает с активной персоной — новая версия не создаётся", async () => {
    await repo.ensureFromSource(SOURCE);
    const again = await repo.ensureFromSource(SOURCE);
    expect(again.version).toBe(1);
    const rows = await pool.query<{ count: string }>("SELECT count(*)::text AS count FROM personas");
    expect(Number(rows.rows[0]!.count)).toBe(1);
  });

  it("изменённый файл — новая версия активна, прежняя остаётся в истории", async () => {
    await repo.ensureFromSource(SOURCE);
    const updated = await repo.ensureFromSource({ ...SOURCE, description: "новое описание из файла" });
    expect(updated.version).toBe(2);
    expect(updated.description).toBe("новое описание из файла");

    const active = await repo.active();
    expect(active.version).toBe(2);

    const rows = await pool.query<{ version: number; active: boolean }>(
      "SELECT version, active FROM personas ORDER BY version",
    );
    expect(rows.rows).toEqual([
      { version: 1, active: false },
      { version: 2, active: true },
    ]);
  });

  it("смена только чисел настроения — тоже новая версия", async () => {
    await repo.ensureFromSource(SOURCE);
    const updated = await repo.ensureFromSource({
      ...SOURCE,
      moodBaseline: { valence: 0.2, arousal: 0.4, socialEnergy: 0.6 },
    });
    expect(updated.version).toBe(2);
    expect(updated.moodBaseline.valence).toBe(0.2);
  });

  it("возврат файла к прежнему тексту создаёт новую версию, а не переактивирует старую", async () => {
    await repo.ensureFromSource(SOURCE);
    await repo.ensureFromSource({ ...SOURCE, description: "временное описание" });
    const reverted = await repo.ensureFromSource(SOURCE);
    expect(reverted.version).toBe(3);
    expect(reverted.description).toBe("описание из файла");
  });
});

afterAll(async () => {
  await pool.end();
});
