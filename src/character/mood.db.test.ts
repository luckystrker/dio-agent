// Интеграционный тест (живой PostgreSQL, база dio_test).
// §7.2: MoodRepo — singleton-строка mood_state.
// - readDecayed: пустая таблица = baseline persona; затухание по elapsed
//   (current = baseline + (stored - baseline) * e^(-λt)); write-back
//   затухнувшего значения (следующая дельта ложится на свежую базу).
// - applyDelta: применяется в транзакции применения, результат в границах
//   полей (-1..1 / 0..1 / 0..1), строка создаётся из пустой таблицы.
// - Второй пояс идемпотентности применения (1.7): UNIQUE(work_item_id,
//   op_index) на state_notes — повторное применение того же хода не дублирует.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Pool } from "pg";
import { MoodRepo, MOOD_DECAY_LAMBDA_PER_HOUR, decayMood, type Mood } from "./mood.js";
import { withTransaction } from "../runtime/transactions.js";
import { applyDeltas, type RuntimeDelta } from "../runtime/deltas.js";
import { WorkQueueRepo } from "../runtime/work-queue.js";
import { cleanTables, createTestPool } from "../test-support/db.js";

const BASELINE: Mood = { valence: 0.1, arousal: 0.4, socialEnergy: 0.6 };
const HOUR_MS = 3_600_000;

let pool: Pool;
let repo: MoodRepo;
let workRepo: WorkQueueRepo;

beforeEach(async () => {
  pool = createTestPool();
  repo = new MoodRepo(pool);
  workRepo = new WorkQueueRepo(pool);
  await cleanTables(pool);
  await pool.query("DELETE FROM mood_state");
});

afterEach(async () => {
  // mood_state не чистится ничем другим: не оставляем её соседям по базе
  await pool.query("DELETE FROM mood_state").catch(() => undefined);
  await pool.end();
});

interface MoodRow {
  valence: string;
  arousal: string;
  social_energy: string;
  updated_at: Date;
}

async function rawRow(): Promise<MoodRow | null> {
  const result = await pool.query<MoodRow>(
    "SELECT valence, arousal, social_energy, updated_at FROM mood_state WHERE id = 1",
  );
  return result.rows[0] ?? null;
}

/** Записать stored-настроение с updated_at на `hoursAgo` часов назад. */
async function plantMood(mood: Mood, hoursAgo: number): Promise<void> {
  await pool.query(
    `INSERT INTO mood_state (id, valence, arousal, social_energy, updated_at)
     VALUES (1, $1, $2, $3, now() - ($4::bigint * interval '1 millisecond'))
     ON CONFLICT (id) DO UPDATE SET valence = EXCLUDED.valence, arousal = EXCLUDED.arousal,
       social_energy = EXCLUDED.social_energy, updated_at = EXCLUDED.updated_at`,
    [mood.valence, mood.arousal, mood.socialEnergy, Math.round(hoursAgo * HOUR_MS)],
  );
}

describe("MoodRepo.readDecayed (§7.2)", () => {
  it("пустая таблица — настроение равно baseline persona, строка не создаётся", async () => {
    const { mood, updatedAt } = await repo.readDecayed(BASELINE);

    expect(mood).toEqual(BASELINE);
    expect(updatedAt).toBeInstanceOf(Date);
    expect(await rawRow()).toBeNull();
  });

  it("затухание по времени: 3 часа назад — каждое поле строго между stored и baseline", async () => {
    const stored: Mood = { valence: 0.9, arousal: 0.95, socialEnergy: 0.05 };
    await plantMood(stored, 3);

    const { mood } = await repo.readDecayed(BASELINE);

    // λ=0.5/ч, 3 ч → factor = e^-1.5 ≈ 0.223: отклонение сжалось, но не исчезло
    const expected = decayMood(stored, BASELINE, 3 * HOUR_MS, MOOD_DECAY_LAMBDA_PER_HOUR);
    expect(mood.valence).toBeCloseTo(expected.valence, 4);
    expect(mood.arousal).toBeCloseTo(expected.arousal, 4);
    expect(mood.socialEnergy).toBeCloseTo(expected.socialEnergy, 4);

    // строго между stored и baseline по каждому полю
    for (const field of ["valence", "arousal", "socialEnergy"] as const) {
      const lo = Math.min(stored[field], BASELINE[field]);
      const hi = Math.max(stored[field], BASELINE[field]);
      expect(mood[field]).toBeGreaterThan(lo);
      expect(mood[field]).toBeLessThan(hi);
    }
  });

  it("write-back: затухнувшее значение реально записано в mood_state с обновлённым updated_at", async () => {
    const stored: Mood = { valence: 0.9, arousal: 0.95, socialEnergy: 0.05 };
    await plantMood(stored, 3);

    const before = Date.now();
    const first = await repo.readDecayed(BASELINE);
    const row = await rawRow();

    expect(row).not.toBeNull();
    expect(Number(row!.valence)).toBeCloseTo(first.mood.valence, 4);
    expect(Number(row!.arousal)).toBeCloseTo(first.mood.arousal, 4);
    expect(Number(row!.social_energy)).toBeCloseTo(first.mood.socialEnergy, 4);
    // updated_at сдвинут на «сейчас», а не остался 3 часа назад
    expect(row!.updated_at.getTime()).toBeGreaterThanOrEqual(before - 1_000);

    // следствие write-back: немедленное повторное чтение (elapsed ≈ 0)
    // возвращает то же значение — затухание не применяется дважды к старой базе
    const second = await repo.readDecayed(BASELINE);
    expect(second.mood.valence).toBeCloseTo(first.mood.valence, 3);
    expect(second.mood.socialEnergy).toBeCloseTo(first.mood.socialEnergy, 3);
  });
});

describe("MoodRepo.applyDelta (§7.2: применение в транзакции применения)", () => {
  it("пустая таблица: строка создаётся от baseline, дельта прибавляется по каждому полю", async () => {
    const next = await withTransaction(pool, (client) =>
      repo.applyDelta(client, BASELINE, { valence: 0.3, arousal: -0.3, socialEnergy: 0.3 }),
    );

    expect(next).toEqual({ valence: 0.4, arousal: 0.1, socialEnergy: 0.9 });
    const row = await rawRow();
    expect(Number(row!.valence)).toBeCloseTo(0.4, 6);
    expect(Number(row!.arousal)).toBeCloseTo(0.1, 6);
    expect(Number(row!.social_energy)).toBeCloseTo(0.9, 6);
  });

  it("клампы границ: результат не выходит из -1..1 / 0..1 / 0..1", async () => {
    // stored у самых краёв + дельта за край — итог ровно на границе
    await plantMood({ valence: 0.95, arousal: 0.05, socialEnergy: 0.95 }, 0);
    const up = await withTransaction(pool, (client) =>
      repo.applyDelta(client, BASELINE, { valence: 0.3, socialEnergy: 0.3 }),
    );
    expect(up.valence).toBeCloseTo(1, 6);
    expect(up.socialEnergy).toBeCloseTo(1, 6);

    await plantMood({ valence: -0.95, arousal: 0.05, socialEnergy: 0.05 }, 0);
    const down = await withTransaction(pool, (client) =>
      repo.applyDelta(client, BASELINE, { valence: -0.3, arousal: -0.3, socialEnergy: -0.3 }),
    );
    expect(down.valence).toBeCloseTo(-1, 6);
    expect(down.arousal).toBeCloseTo(0, 6);
    expect(down.socialEnergy).toBeCloseTo(0, 6);
  });

  it("applyDelta сам затухает сохранённое: дельта ложится на decayed-базу, а не на протухшую stored", async () => {
    const stored: Mood = { valence: 0.9, arousal: 0.95, socialEnergy: 0.05 };
    await plantMood(stored, 3); // без промежуточного readDecayed

    const next = await withTransaction(pool, (client) =>
      repo.applyDelta(client, BASELINE, { valence: -0.3 }),
    );

    const decayedValence = decayMood(stored, BASELINE, 3 * HOUR_MS).valence;
    // в хранилище 3 знака после запятой (round3 в applyDelta)
    expect(next.valence).toBeCloseTo(decayedValence - 0.3, 2);
  });

  it("частичная дельта: незатронутые поля пересчитываются (decay), но не сдвигаются", async () => {
    const stored: Mood = { valence: 0.9, arousal: 0.4, socialEnergy: 0.6 };
    await plantMood(stored, 2);

    const next = await withTransaction(pool, (client) =>
      repo.applyDelta(client, BASELINE, { valence: -0.3 }),
    );

    expect(next.valence).toBeCloseTo(decayMood(stored, BASELINE, 2 * HOUR_MS).valence - 0.3, 2);
    // arousal/socialEnergy на baseline — дельта их не трогала
    expect(next.arousal).toBeCloseTo(BASELINE.arousal, 3);
    expect(next.socialEnergy).toBeCloseTo(BASELINE.socialEnergy, 3);
  });
});

describe("второй пояс идемпотентности применения (1.7): state_notes", () => {
  const deltas: RuntimeDelta[] = [
    { type: "append_note", text: "mood: заметка хода" },
    { type: "append_note", text: "вторая заметка" },
  ];

  it("повторное применение того же хода (те же op_index) не дублирует заметки", async () => {
    const workId = await workRepo.enqueue("direct", { note: "ход" });

    await withTransaction(pool, (client) => applyDeltas(client, workId, deltas));
    // «повторный прогон обработчика с тем же work_item_id» (сбой после commit)
    await withTransaction(pool, (client) => applyDeltas(client, workId, deltas));

    const notes = await pool.query<{ text: string; op_index: number }>(
      "SELECT text, op_index FROM state_notes WHERE work_item_id = $1 ORDER BY op_index",
      [workId],
    );
    expect(notes.rows.map((row) => [row.op_index, row.text])).toEqual([
      [0, "mood: заметка хода"],
      [1, "вторая заметка"],
    ]);
  });

  it("другой work_item со своим op_index=0 не конфликтует", async () => {
    const first = await workRepo.enqueue("direct", {});
    const second = await workRepo.enqueue("direct", {});

    await withTransaction(pool, (client) => applyDeltas(client, first, deltas.slice(0, 1)));
    await withTransaction(pool, (client) => applyDeltas(client, second, deltas.slice(0, 1)));

    const count = await pool.query<{ count: string }>(
      "SELECT count(*)::text AS count FROM state_notes",
    );
    expect(Number(count.rows[0]!.count)).toBe(2);
  });
});
