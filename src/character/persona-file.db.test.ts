import { readFileSync } from "node:fs";
import { afterAll, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { PersonaRepo } from "./persona.js";
import { parsePersonaFile } from "./persona-file.js";
import { createTestPool } from "../test-support/db.js";

// §7.1 (независимая приёмка по спецификации, не по коду): файл — источник истины.
// PersonaRepo.ensureFromSource(source): совпадение со активной персоной по ВСЕМ
// полям (name, description, speechStyle, values как список, moodBaseline все три
// числа, seedBio включая null) — ничего не меняется; отличие — новая версия
// max(version)+1, она активна, прежние деактивированы, история сохраняется
// (append-only: возврат файла к прежнему тексту — новая версия, не реактивация).

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

const VERSION_COUNT = async (): Promise<number> => {
  const rows = await pool.query<{ count: string }>("SELECT count(*)::text AS count FROM personas");
  return Number(rows.rows[0]!.count);
};

beforeEach(async () => {
  await pool.query("DELETE FROM personas");
});

describe("ensureFromSource §7.1 — сравнение идёт по каждому полю (независимая приёмка)", () => {
  // отличие ТОЛЬКО в одном поле должно создавать новую версию — по каждому полю
  const oneFieldDiffers: Array<[string, object]> = [
    ["name", { name: "Дио Прайм" }],
    ["description", { description: "другое описание" }],
    ["speechStyle", { speechStyle: "иначе формулирую" }],
    ["values (состав)", { values: ["честность", "любопытство", "умеренность"] }],
    ["moodBaseline.valence", { moodBaseline: { valence: 0.2, arousal: 0.4, socialEnergy: 0.6 } }],
    ["moodBaseline.arousal", { moodBaseline: { valence: 0.1, arousal: 0.5, socialEnergy: 0.6 } }],
    ["moodBaseline.socialEnergy", { moodBaseline: { valence: 0.1, arousal: 0.4, socialEnergy: 0.7 } }],
    ["seedBio: текст → null", { seedBio: null }],
  ];

  it.each(oneFieldDiffers)("отличие только в %s — версия 2 активна, версия 1 остаётся в истории", async (_label, delta) => {
    await repo.ensureFromSource(SOURCE);
    const next = await repo.ensureFromSource({ ...SOURCE, ...delta });
    expect(next.version).toBe(2);

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

  it("появление биографии (seedBio: null → текст) — тоже новая версия, текст сохраняется", async () => {
    await repo.ensureFromSource({ ...SOURCE, seedBio: null });
    const next = await repo.ensureFromSource(SOURCE);
    expect(next.version).toBe(2);
    expect(next.seedBio).toBe(SOURCE.seedBio);
  });

  it("источник с seedBio: null дважды подряд — версия не растёт (null участвует в сравнении)", async () => {
    await repo.ensureFromSource({ ...SOURCE, seedBio: null });
    const again = await repo.ensureFromSource({ ...SOURCE, seedBio: null });
    expect(again.version).toBe(1);
    expect(await VERSION_COUNT()).toBe(1);
  });
});

describe("ensureFromSource §7.1 — значения и история (независимая приёмка)", () => {
  it("порядок ценностей входит в сравнение: values сравниваются как список, не как множество", async () => {
    await repo.ensureFromSource(SOURCE);
    const swapped = await repo.ensureFromSource({ ...SOURCE, values: ["любопытство", "честность"] });
    expect(swapped.version).toBe(2);
    expect(swapped.values).toEqual(["любопытство", "честность"]);
  });

  it("новая версия получает номер max(version)+1 по всей таблице, а не «число строк + 1»", async () => {
    // чужие старые версии (не активные) с большими номерами
    await pool.query(
      `INSERT INTO personas (version, active, name, description, speech_style, values,
                             mood_valence, mood_arousal, mood_social_energy)
       VALUES (3, FALSE, 'старая-3', 'x', 'x', '["x"]'::jsonb, 0, 0.5, 0.5),
              (7, FALSE, 'старая-7', 'y', 'y', '["y"]'::jsonb, 0, 0.5, 0.5)`,
    );
    const created = await repo.ensureFromSource(SOURCE);
    expect(created.version).toBe(8);

    const rows = await pool.query<{ version: number; active: boolean }>(
      "SELECT version, active FROM personas ORDER BY version",
    );
    expect(rows.rows).toEqual([
      { version: 3, active: false },
      { version: 7, active: false },
      { version: 8, active: true },
    ]);
  });

  it("история append-only: три разных источника — три строки, активна только последняя", async () => {
    await repo.ensureFromSource(SOURCE);
    await repo.ensureFromSource({ ...SOURCE, description: "второе описание" });
    await repo.ensureFromSource({ ...SOURCE, description: "третье описание" });

    const rows = await pool.query<{ version: number; active: boolean; description: string }>(
      "SELECT version, active, description FROM personas ORDER BY version",
    );
    expect(rows.rows).toEqual([
      { version: 1, active: false, description: "описание из файла" },
      { version: 2, active: false, description: "второе описание" },
      { version: 3, active: true, description: "третье описание" },
    ]);
  });

  it("пустая база + источник без биографии (seedBio: null) — версия 1, seedBio читается как null", async () => {
    const persona = await repo.ensureFromSource({ ...SOURCE, seedBio: null });
    expect(persona.version).toBe(1);
    expect(persona.seedBio).toBeNull();
    const reread = await repo.active();
    expect(reread.seedBio).toBeNull();
  });

  it("сквозной сценарий «файл — источник истины»: реальный config/persona.md при повторном старте не плодит версий", async () => {
    const file = readFileSync(new URL("../../config/persona.md", import.meta.url), "utf8");
    const source = parsePersonaFile(file);

    const first = await repo.ensureFromSource(source);
    const second = await repo.ensureFromSource(parsePersonaFile(readFileSync(new URL("../../config/persona.md", import.meta.url), "utf8")));

    expect(second.version).toBe(first.version);
    expect(await VERSION_COUNT()).toBe(1);

    const active = await repo.active();
    expect(active.name).toBe(source.name);
    expect(active.description).toBe(source.description);
    expect(active.speechStyle).toBe(source.speechStyle);
    expect(active.values).toEqual(source.values);
    expect(active.moodBaseline).toEqual(source.moodBaseline);
    expect(active.seedBio).toBe(source.seedBio);
  });
});

afterAll(async () => {
  await pool.end();
});
