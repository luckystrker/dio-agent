import { describe, expect, it } from "vitest";
import {
  MAX_MOOD_DELTA_PER_TURN,
  clampMoodDelta,
  decayMood,
  describeMood,
  type Mood,
} from "./mood.js";

const BASELINE: Mood = { valence: 0.1, arousal: 0.4, socialEnergy: 0.6 };
const HOUR = 3_600_000;

describe("decayMood (§7.2)", () => {
  it("нулевой elapsed — настроение не меняется", () => {
    const stored: Mood = { valence: 0.8, arousal: 0.9, socialEnergy: 0.2 };
    expect(decayMood(stored, BASELINE, 0)).toEqual(stored);
  });

  it("затухание к базовой линии: около половины отклонения за период полураспада", () => {
    const stored: Mood = { valence: 0.9, arousal: 0.4, socialEnergy: 0.6 };
    // λ=0.5/ч; полураспад ≈ ln2/0.5 ≈ 1.386 ч
    const decayed = decayMood(stored, BASELINE, 1.386 * HOUR);
    expect(decayed.valence).toBeCloseTo(0.1 + (0.9 - 0.1) / 2, 2);
  });

  it("очень долгое время — практически базовая линия", () => {
    const stored: Mood = { valence: -1, arousal: 1, socialEnergy: 0 };
    const decayed = decayMood(stored, BASELINE, 100 * HOUR);
    expect(decayed.valence).toBeCloseTo(BASELINE.valence, 3);
    expect(decayed.arousal).toBeCloseTo(BASELINE.arousal, 3);
    expect(decayed.socialEnergy).toBeCloseTo(BASELINE.socialEnergy, 3);
  });

  it("результат всегда в допустимых границах при любом входе", () => {
    const decayed = decayMood(
      { valence: -1, arousal: 0, socialEnergy: 0 },
      { valence: 0.9, arousal: 0.95, socialEnergy: 0.95 },
      5 * HOUR,
    );
    expect(decayed.valence).toBeLessThanOrEqual(1);
    expect(decayed.valence).toBeGreaterThanOrEqual(-1);
    expect(decayed.arousal).toBeLessThanOrEqual(1);
    expect(decayed.arousal).toBeGreaterThanOrEqual(0);
    expect(decayed.socialEnergy).toBeLessThanOrEqual(1);
  });
});

describe("clampMoodDelta (§7.2: изменение ограничено на ход)", () => {
  it("в пределах лимита проходит как есть", () => {
    expect(clampMoodDelta({ valence: 0.2, arousal: -0.1 })).toEqual({
      valence: 0.2,
      arousal: -0.1,
    });
  });

  it("за пределами — сжимается до ±MAX_MOOD_DELTA_PER_TURN по каждому полю", () => {
    expect(clampMoodDelta({ valence: 5, arousal: -5, socialEnergy: 0.5 })).toEqual({
      valence: MAX_MOOD_DELTA_PER_TURN,
      arousal: -MAX_MOOD_DELTA_PER_TURN,
      socialEnergy: MAX_MOOD_DELTA_PER_TURN,
    });
  });

  it("пустая дельта остаётся пустой (нет изменений — тоже результат)", () => {
    expect(clampMoodDelta({})).toEqual({});
  });
});

describe("describeMood — детерминированный текст", () => {
  it("нейтральное состояние", () => {
    expect(describeMood(BASELINE)).toBe("в ровном настроении");
  });

  it("сдвиг относительно базовой линии виден при небольшой дельте", () => {
    expect(
      describeMood(
        { valence: -0.05, arousal: 0.4, socialEnergy: 0.6 },
        { valence: 0.1, arousal: 0.4, socialEnergy: 0.6 },
      ),
    ).toBe("не в духе");
  });

  it("приподнятое и взолнованное", () => {
    expect(describeMood({ valence: 0.7, arousal: 0.8, socialEnergy: 0.5 })).toBe(
      "в приподнятом настроении, взволнован",
    );
  });

  it("подавленное и мало сил", () => {
    expect(describeMood({ valence: -0.6, arousal: 0.2, socialEnergy: 0.1 })).toBe(
      "в подавленном настроении, спокоен, мало сил на общение",
    );
  });

  it("одинаковый вход — одинаковый текст (детерминированность)", () => {
    const mood: Mood = { valence: 0.5, arousal: 0.5, socialEnergy: 0.8 };
    expect(describeMood(mood)).toBe(describeMood(mood));
  });
});
