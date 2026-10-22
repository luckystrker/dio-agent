// Приёмочный PBT по спецификации 8.1 (§11.1 п.5), не по коду:
// fitHistory — окно недавнего разговора с потолком объёма:
// - самое свежее сообщение попадает всегда (даже если само больше бюджета);
// - если что-то теряется, первыми теряются САМЫЕ СТАРЫЕ: результат —
//   «хвост» входа, порядок сохраняется, содержимое не искажается;
// - больший бюджет не выбрасывает больше сообщений, чем меньший;
// - пустой вход даёт пустой результат.
// Входы — произвольные строки (в том числе пустые), обе роли.

import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { fitHistory, type HistoryMessage } from "./recent-history.js";

const messageArb: fc.Arbitrary<HistoryMessage> = fc.record({
  role: fc.constantFrom("user", "assistant"),
  content: fc.string({ minLength: 0, maxLength: 120 }),
});

const historyArb = fc.array(messageArb, { minLength: 0, maxLength: 30 });
const budgetArb = fc.integer({ min: 0, max: 400 });

/** Результат — хвост входа той же длины: выброшены только первые k. */
function expectTrailingSlice(input: HistoryMessage[], fitted: HistoryMessage[]): void {
  expect(fitted.length).toBeLessThanOrEqual(input.length);
  const kept = input.slice(input.length - fitted.length);
  expect(fitted).toEqual(kept);
}

describe("PBT: fitHistory (8.1, §11.1 п.5 — окно недавнего разговора)", () => {
  it("самое свежее сообщение попадает всегда: вход непуст ⇒ результат непуст и кончается им же", async () => {
    await fc.assert(
      fc.property(
        fc.array(messageArb, { minLength: 1, maxLength: 30 }),
        budgetArb,
        (history, maxChars) => {
          const fitted = fitHistory(history, maxChars);
          expect(fitted.length).toBeGreaterThanOrEqual(1);
          expect(fitted[fitted.length - 1]).toEqual(history[history.length - 1]);
        },
      ),
      { numRuns: 500 },
    );
  });

  it("старые теряются первыми, порядок сохраняется: результат — хвост входа без искажений", async () => {
    await fc.assert(
      fc.property(historyArb, budgetArb, (history, maxChars) => {
        expectTrailingSlice(history, fitHistory(history, maxChars));
      }),
      { numRuns: 500 },
    );
  });

  it("бюджет больше — выброшено не больше: длина результата монотонна по бюджету", async () => {
    await fc.assert(
      fc.property(historyArb, fc.integer({ min: 0, max: 200 }), fc.integer({ min: 0, max: 200 }), (
        history,
        smallRaw,
        extra,
      ) => {
        const small = fitHistory(history, smallRaw);
        const large = fitHistory(history, smallRaw + extra);
        expect(large.length).toBeGreaterThanOrEqual(small.length);
        // и больший результат — тоже хвост входа, продолжающий тот же хвост
        expectTrailingSlice(history, large);
        expect(large.slice(large.length - small.length)).toEqual(small);
      }),
      { numRuns: 500 },
    );
  });

  it("всё помещается (сумма длин content ≤ бюджета) — не выброшено ничего", async () => {
    await fc.assert(
      fc.property(historyArb, (history) => {
        const total = history.reduce((sum, m) => sum + m.content.length, 0);
        expect(fitHistory(history, total)).toEqual(history);
      }),
      { numRuns: 500 },
    );
  });

  it("пустой вход — пустой результат при любом бюджете", async () => {
    await fc.assert(
      fc.property(budgetArb, (maxChars) => {
        expect(fitHistory([], maxChars)).toEqual([]);
      }),
      { numRuns: 100 },
    );
  });

  it("контрпример-якорь: при нехватке бюджета падает именно голова, свежий хвост цел", () => {
    const history: HistoryMessage[] = [
      { role: "user", content: "a".repeat(50) },
      { role: "assistant", content: "b".repeat(50) },
      { role: "user", content: "c".repeat(50) },
    ];
    // 60 < 100 (сумма b+c): влезает только свежее; 120 ≥ 100: хвост [b, c] цел
    expect(fitHistory(history, 60)).toEqual([{ role: "user", content: "c".repeat(50) }]);
    expect(fitHistory(history, 120)).toEqual([
      { role: "assistant", content: "b".repeat(50) },
      { role: "user", content: "c".repeat(50) },
    ]);
    expect(fitHistory(history, 1_000_000)).toEqual(history);
  });
});
