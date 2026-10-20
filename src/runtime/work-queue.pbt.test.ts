import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { decideRecoveryAction } from "./work-queue.js";
import { WORK_MAX_ATTEMPTS } from "./priorities.js";

// PBT по §12.2/§5.3: работа в running после сбоя либо возвращается в очередь,
// либо честно помечается пропуском. Граница — attempts == maxAttempts.

const attemptsArb = fc.nat({ max: 100 });
const maxArb = fc.integer({ min: 1, max: 100 });

describe("PBT: decideRecoveryAction", () => {
  it("для любых attempts < maxAttempts решение — requeue", async () => {
    await fc.assert(
      fc.property(attemptsArb, maxArb, (attempts, max) => {
        fc.pre(attempts < max);
        expect(decideRecoveryAction(attempts, max)).toBe("requeue");
      }),
    );
  });

  it("для любых attempts >= maxAttempts решение — skip (терминальный пропуск)", async () => {
    await fc.assert(
      fc.property(attemptsArb, maxArb, fc.integer({ min: 0, max: 50 }), (attempts, max, over) => {
        fc.pre(attempts >= max);
        expect(decideRecoveryAction(attempts + over, max)).toBe("skip");
      }),
    );
  });

  it("решение принимает ровно два значения — «вечно running» не бывает", async () => {
    await fc.assert(
      fc.property(attemptsArb, maxArb, (attempts, max) => {
        const action = decideRecoveryAction(attempts, max);
        expect(["requeue", "skip"]).toContain(action);
      }),
    );
  });

  it("монотонность: с ростом attempts решение не возвращается от skip к requeue", async () => {
    await fc.assert(
      fc.property(attemptsArb, maxArb, fc.integer({ min: 1, max: 20 }), (attempts, max, step) => {
        const before = decideRecoveryAction(attempts, max);
        const after = decideRecoveryAction(attempts + step, max);
        if (before === "skip") expect(after).toBe("skip");
      }),
    );
  });

  it("с дефолтным WORK_MAX_ATTEMPTS граница точна: < max → requeue, >= max → skip", async () => {
    await fc.assert(
      fc.property(fc.integer({ min: 0, max: WORK_MAX_ATTEMPTS + 5 }), (attempts) => {
        const expected = attempts < WORK_MAX_ATTEMPTS ? "requeue" : "skip";
        expect(decideRecoveryAction(attempts)).toBe(expected);
      }),
    );
  });
});
