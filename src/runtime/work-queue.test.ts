import { describe, expect, it } from "vitest";
import { decideRecoveryAction } from "./work-queue.js";
import { WORK_MAX_ATTEMPTS, WORK_PRIORITIES } from "./priorities.js";

describe("decideRecoveryAction", () => {
  it("пока попытки не исчерпаны — возврат в очередь", () => {
    expect(decideRecoveryAction(1)).toBe("requeue");
    expect(decideRecoveryAction(WORK_MAX_ATTEMPTS - 1)).toBe("requeue");
  });

  it("при исчерпании попыток — терминальный пропуск", () => {
    expect(decideRecoveryAction(WORK_MAX_ATTEMPTS)).toBe("skip");
    expect(decideRecoveryAction(WORK_MAX_ATTEMPTS + 5)).toBe("skip");
  });

  it("кастомный максимум уважается", () => {
    expect(decideRecoveryAction(1, 1)).toBe("skip");
    expect(decideRecoveryAction(1, 5)).toBe("requeue");
  });
});

describe("WORK_PRIORITIES", () => {
  it("порядок приоритетов соответствует §5.2", () => {
    expect(WORK_PRIORITIES.direct).toBeGreaterThan(WORK_PRIORITIES.group);
    expect(WORK_PRIORITIES.group).toBeGreaterThan(WORK_PRIORITIES.intention);
    expect(WORK_PRIORITIES.intention).toBeGreaterThan(WORK_PRIORITIES.exploration);
    expect(WORK_PRIORITIES.exploration).toBeGreaterThan(WORK_PRIORITIES.consolidation);
  });
});
