import type { Pool, PoolClient } from "pg";

// Mood (§7.2): три числа с детерминированным затуханием к базовой линии
// persona. Фоновый LLM для decay не нужен. Изменения ограничены на ход и
// требуют основания; конкретная обида/радость живёт в памяти, а не в числе.

export interface Mood {
  valence: number; // -1..1
  arousal: number; // 0..1
  socialEnergy: number; // 0..1
}

/** Период полураспада отклонения ≈ 1.4 часа. */
export const MOOD_DECAY_LAMBDA_PER_HOUR = 0.5;

/** Максимальное изменение параметра за один ход (§7.2). */
export const MAX_MOOD_DELTA_PER_TURN = 0.3;

function clamp(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, value));
}

/** Затухание к базовой линии: current = baseline + (stored - baseline) * e^(-λt). */
export function decayMood(
  stored: Mood,
  baseline: Mood,
  elapsedMs: number,
  lambdaPerHour: number = MOOD_DECAY_LAMBDA_PER_HOUR,
): Mood {
  const factor = Math.exp((-lambdaPerHour * elapsedMs) / 3_600_000);
  return {
    valence: clamp(baseline.valence + (stored.valence - baseline.valence) * factor, -1, 1),
    arousal: clamp(baseline.arousal + (stored.arousal - baseline.arousal) * factor, 0, 1),
    socialEnergy: clamp(
      baseline.socialEnergy + (stored.socialEnergy - baseline.socialEnergy) * factor,
      0,
      1,
    ),
  };
}

/** Сжатие предложенной модели дельты до ±MAX_MOOD_DELTA_PER_TURN по параметру. */
export function clampMoodDelta(delta: Partial<Mood>): Partial<Mood> {
  const clamped: Partial<Mood> = {};
  if (delta.valence !== undefined) {
    clamped.valence = clamp(delta.valence, -MAX_MOOD_DELTA_PER_TURN, MAX_MOOD_DELTA_PER_TURN);
  }
  if (delta.arousal !== undefined) {
    clamped.arousal = clamp(delta.arousal, -MAX_MOOD_DELTA_PER_TURN, MAX_MOOD_DELTA_PER_TURN);
  }
  if (delta.socialEnergy !== undefined) {
    clamped.socialEnergy = clamp(
      delta.socialEnergy,
      -MAX_MOOD_DELTA_PER_TURN,
      MAX_MOOD_DELTA_PER_TURN,
    );
  }
  return clamped;
}

/** Детерминированное текстовое представление (§7.2) — в контекст хода.
 *  Сравниваем с базовой линией persona, а не с нулём: у Дио baseline valence 0.1,
 *  и абсолютные пороги прятали небольшие, но заметные сдвиги. */
export function describeMood(
  mood: Mood,
  baseline: Mood = { valence: 0, arousal: 0.5, socialEnergy: 0.5 },
): string {
  const parts: string[] = [];
  const v = mood.valence - baseline.valence;
  if (v > 0.4) parts.push("в приподнятом настроении");
  else if (v > 0.12) parts.push("чуть оживлён");
  else if (v < -0.4) parts.push("в подавленном настроении");
  else if (v < -0.12) parts.push("не в духе");
  else parts.push("в ровном настроении");

  const a = mood.arousal - baseline.arousal;
  if (a > 0.2) parts.push("взволнован");
  else if (a < -0.15) parts.push("спокоен");

  const s = mood.socialEnergy - baseline.socialEnergy;
  if (s < -0.2) parts.push("мало сил на общение");
  else if (s > 0.15) parts.push("полон желания общаться");

  return parts.join(", ");
}

export interface StoredMood {
  mood: Mood;
  updatedAt: Date;
}

export class MoodRepo {
  constructor(private readonly pool: Pool) {}

  /**
   * Читает настроение с затуханием от времени последнего обновления
   * и записывает затухнувшее значение обратно (следующая дельта ложится
   * на свежую базу). Отсутствие строки = базовая линия persona.
   */
  async readDecayed(baseline: Mood): Promise<StoredMood> {
    const row = await this.pool.query<{ valence: string; arousal: string; social_energy: string; updated_at: Date }>(
      "SELECT valence, arousal, social_energy, updated_at FROM mood_state WHERE id = 1",
    );
    const current = row.rows[0];
    if (!current) return { mood: { ...baseline }, updatedAt: new Date() };

    const stored: Mood = {
      valence: Number(current.valence),
      arousal: Number(current.arousal),
      socialEnergy: Number(current.social_energy),
    };
    const elapsed = Date.now() - current.updated_at.getTime();
    const decayed = decayMood(stored, baseline, elapsed);
    await this.pool.query(
      "UPDATE mood_state SET valence = $1, arousal = $2, social_energy = $3, updated_at = now() WHERE id = 1",
      [decayed.valence, decayed.arousal, decayed.socialEnergy],
    );
    return { mood: decayed, updatedAt: new Date() };
  }

  /** Применение дельты хода ВНУТРИ транзакции применения (после сжатия). */
  async applyDelta(
    client: PoolClient,
    baseline: Mood,
    delta: Partial<Mood>,
  ): Promise<Mood> {
    const existing = await client.query<{ valence: string; arousal: string; social_energy: string; updated_at: Date }>(
      "SELECT valence, arousal, social_energy, updated_at FROM mood_state WHERE id = 1",
    );
    const row = existing.rows[0];
    const stored: Mood = row
      ? {
          valence: Number(row.valence),
          arousal: Number(row.arousal),
          socialEnergy: Number(row.social_energy),
        }
      : { ...baseline };
    const elapsed = row ? Date.now() - row.updated_at.getTime() : 0;
    const base = decayMood(stored, baseline, elapsed);

    // округление до 3 знаков: избавляет от артефактов плавающей точки в хранилище
    const round3 = (value: number): number => Math.round(value * 1000) / 1000;
    const next: Mood = {
      valence: round3(clamp(base.valence + (delta.valence ?? 0), -1, 1)),
      arousal: round3(clamp(base.arousal + (delta.arousal ?? 0), 0, 1)),
      socialEnergy: round3(clamp(base.socialEnergy + (delta.socialEnergy ?? 0), 0, 1)),
    };
    await client.query(
      `INSERT INTO mood_state (id, valence, arousal, social_energy, updated_at)
       VALUES (1, $1, $2, $3, now())
       ON CONFLICT (id) DO UPDATE
         SET valence = EXCLUDED.valence, arousal = EXCLUDED.arousal,
             social_energy = EXCLUDED.social_energy, updated_at = now()`,
      [next.valence, next.arousal, next.socialEnergy],
    );
    return next;
  }
}
