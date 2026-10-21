// PBT (гейт без БД). §7.2: свойства затухания и сжатия дельты настроения:
// - clampMoodDelta: ЛЮБАЯ дельта (в том числе за пределами полей) сжимается
//   в ±MAX_MOOD_DELTA_PER_TURN по каждому присутствующему полю; знак и
//   нулевые поля сохраняются;
// - decayMood: монотонность (больше elapsed — строго не дальше от baseline,
//   результат между предыдущим состоянием и baseline), результат всегда
//   в допустимых границах полей, elapsed=0 — тождество.

import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { MAX_MOOD_DELTA_PER_TURN, clampMoodDelta, decayMood } from "./mood.js";

const HOUR_MS = 3_600_000;

const moodArb = fc.record({
  valence: fc.double({ min: -1, max: 1, noNaN: true }),
  arousal: fc.double({ min: 0, max: 1, noNaN: true }),
  socialEnergy: fc.double({ min: 0, max: 1, noNaN: true }),
});

/** Дельта любая: за пределами полей и лимита хода, с пропусками полей. */
const wildDeltaArb = fc.record({
  valence: fc.option(fc.double({ min: -1e6, max: 1e6, noNaN: true }), { nil: undefined }),
  arousal: fc.option(fc.double({ min: -1e6, max: 1e6, noNaN: true }), { nil: undefined }),
  socialEnergy: fc.option(fc.double({ min: -1e6, max: 1e6, noNaN: true }), { nil: undefined }),
});

const FIELDS = ["valence", "arousal", "socialEnergy"] as const;

describe("PBT: clampMoodDelta сжимает любую дельту в лимит хода (§7.2)", () => {
  it("каждое присутствующее поле результата в [-MAX, +MAX]; отсутствующее остаётся отсутствующим", async () => {
    await fc.assert(
      fc.property(wildDeltaArb, (delta) => {
        const clamped = clampMoodDelta(delta);
        for (const field of FIELDS) {
          if (delta[field] === undefined) {
            expect(clamped[field]).toBeUndefined();
          } else {
            expect(clamped[field]).toBeDefined();
            expect(Math.abs(clamped[field]!)).toBeLessThanOrEqual(MAX_MOOD_DELTA_PER_TURN + 1e-12);
          }
        }
      }),
      { numRuns: 500 },
    );
  });

  it("сжатие не разворачивает знак и не увеличивает модуль дельты", async () => {
    await fc.assert(
      fc.property(wildDeltaArb, (delta) => {
        const clamped = clampMoodDelta(delta);
        for (const field of FIELDS) {
          const before = delta[field];
          const after = clamped[field];
          if (before === undefined || after === undefined) continue;
          expect(Math.abs(after)).toBeLessThanOrEqual(Math.abs(before) + 1e-12);
          if (before > 0) expect(after).toBeGreaterThanOrEqual(0);
          if (before < 0) expect(after).toBeLessThanOrEqual(0);
        }
      }),
      { numRuns: 500 },
    );
  });

  it("малые дельты (внутри лимита) проходят без искажения", async () => {
    const smallDeltaArb = fc.record({
      valence: fc.option(fc.double({ min: -MAX_MOOD_DELTA_PER_TURN, max: MAX_MOOD_DELTA_PER_TURN, noNaN: true }), { nil: undefined }),
      arousal: fc.option(fc.double({ min: -MAX_MOOD_DELTA_PER_TURN, max: MAX_MOOD_DELTA_PER_TURN, noNaN: true }), { nil: undefined }),
      socialEnergy: fc.option(fc.double({ min: -MAX_MOOD_DELTA_PER_TURN, max: MAX_MOOD_DELTA_PER_TURN, noNaN: true }), { nil: undefined }),
    });
    await fc.assert(
      fc.property(smallDeltaArb, (delta) => {
        expect(clampMoodDelta(delta)).toEqual(delta);
      }),
      { numRuns: 500 },
    );
  });
});

describe("PBT: decayMood — монотонное затухание к baseline (§7.2)", () => {
  const elapsedArb = fc.double({ min: 0, max: 100 * HOUR_MS, noNaN: true });
  const stepArb = fc.double({ min: 1, max: 50 * HOUR_MS, noNaN: true });

  it("больше elapsed — не дальше от baseline по каждому полю; результат между прежним состоянием и baseline", async () => {
    await fc.assert(
      fc.property(moodArb, moodArb, elapsedArb, stepArb, (stored, baseline, t1, dt) => {
        const sooner = decayMood(stored, baseline, t1);
        const later = decayMood(stored, baseline, t1 + dt);
        for (const field of FIELDS) {
          const distanceSooner = Math.abs(sooner[field] - baseline[field]);
          const distanceLater = Math.abs(later[field] - baseline[field]);
          expect(distanceLater).toBeLessThanOrEqual(distanceSooner + 1e-9);
          const lo = Math.min(sooner[field], baseline[field]);
          const hi = Math.max(sooner[field], baseline[field]);
          expect(later[field]).toBeGreaterThanOrEqual(lo - 1e-9);
          expect(later[field]).toBeLessThanOrEqual(hi + 1e-9);
        }
      }),
      { numRuns: 500 },
    );
  });

  it("результат всегда в границах полей: valence -1..1, arousal/socialEnergy 0..1", async () => {
    await fc.assert(
      fc.property(moodArb, moodArb, elapsedArb, (stored, baseline, elapsed) => {
        const decayed = decayMood(stored, baseline, elapsed);
        expect(decayed.valence).toBeGreaterThanOrEqual(-1 - 1e-12);
        expect(decayed.valence).toBeLessThanOrEqual(1 + 1e-12);
        expect(decayed.arousal).toBeGreaterThanOrEqual(0 - 1e-12);
        expect(decayed.arousal).toBeLessThanOrEqual(1 + 1e-12);
        expect(decayed.socialEnergy).toBeGreaterThanOrEqual(0 - 1e-12);
        expect(decayed.socialEnergy).toBeLessThanOrEqual(1 + 1e-12);
      }),
      { numRuns: 500 },
    );
  });

  it("elapsed = 0 — тождество: затухание не меняет настроение", async () => {
    await fc.assert(
      fc.property(moodArb, moodArb, (stored, baseline) => {
        const decayed = decayMood(stored, baseline, 0);
        expect(decayed.valence).toBeCloseTo(stored.valence, 12);
        expect(decayed.arousal).toBeCloseTo(stored.arousal, 12);
        expect(decayed.socialEnergy).toBeCloseTo(stored.socialEnergy, 12);
      }),
      { numRuns: 500 },
    );
  });

  it("очень долгое время — практически baseline (отклонение исчезающе мало)", async () => {
    await fc.assert(
      fc.property(moodArb, moodArb, (stored, baseline) => {
        const decayed = decayMood(stored, baseline, 10_000 * HOUR_MS);
        for (const field of FIELDS) {
          expect(Math.abs(decayed[field] - baseline[field])).toBeLessThan(1e-9);
        }
      }),
      { numRuns: 200 },
    );
  });
});
