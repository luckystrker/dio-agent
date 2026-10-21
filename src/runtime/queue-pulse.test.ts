// Юнит-тесты чистой функции queuePulseDecision (§20: «рост очереди требует
// предупреждения оператору»): решение об уровне записи о состоянии очереди
// в пульсе. Свойства — по спецификации функции:
//   пустая очередь → "silent";
//   есть глубина, возраст null или ≤ порога → "info";
//   возраст > порога → "warn".
// Файл сознательно отдельный от work-queue.test.ts (не трогаем существующие тесты).

import { describe, expect, it } from "vitest";
import { queuePulseDecision } from "./work-queue.js";

describe("queuePulseDecision: уровень записи о состоянии очереди в пульсе (§20)", () => {
  it("пустая очередь → silent: пульс не шумит, когда работы нет", () => {
    expect(queuePulseDecision({ pending: 0, oldestPendingAgeSec: null }, 300)).toBe("silent");
  });

  it("непустая очередь с возрастом null → info: глубина есть, но возраст неизвестен", () => {
    expect(queuePulseDecision({ pending: 3, oldestPendingAgeSec: null }, 300)).toBe("info");
  });

  it.each([
    { age: 0, expected: "info", label: "свежая работа" },
    { age: 1, expected: "info", label: "возраст 1 с" },
    { age: 299, expected: "info", label: "порог − 1" },
    { age: 300, expected: "info", label: "ровно на пороге (не warn)" },
    { age: 301, expected: "warn", label: "порог + 1" },
    { age: 3_600, expected: "warn", label: "сильно за порогом" },
  ] as const)(
    "порог 300 с, возраст $age с ($label) → $expected",
    ({ age, expected }) => {
      expect(queuePulseDecision({ pending: 2, oldestPendingAgeSec: age }, 300)).toBe(expected);
    },
  );

  it("ровно на пороге — info при любом пороге (граница «строго больше»)", () => {
    expect(queuePulseDecision({ pending: 1, oldestPendingAgeSec: 0 }, 0)).toBe("info");
    expect(queuePulseDecision({ pending: 1, oldestPendingAgeSec: 60 }, 60)).toBe("info");
    expect(queuePulseDecision({ pending: 1, oldestPendingAgeSec: 61 }, 60)).toBe("warn");
  });

  it("уровень определяется возрастом, а не глубиной: одна старая работа — уже warn, большая глубина без возраста — info", () => {
    expect(queuePulseDecision({ pending: 1, oldestPendingAgeSec: 1_000 }, 300)).toBe("warn");
    expect(queuePulseDecision({ pending: 500, oldestPendingAgeSec: 5 }, 300)).toBe("info");
  });
});
