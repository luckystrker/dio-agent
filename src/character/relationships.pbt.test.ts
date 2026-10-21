// PBT (гейт без БД). §7.3: детерминированное текстовое описание отношений —
// пороги категорий. Свойства: для любого значения familiarity входит ровно
// одна фраза уровня знакомства и уровень монотонен по familiarity; фразы
// симпатии/антипатии появляются только за строгими порогами ±0.3; доверие —
// строгие пороги 0.15/0.6; описание детерминировано.

import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { describeRelationship, type Relationship } from "./relationships.js";

const relationshipArb = fc.record({
  familiarity: fc.double({ min: 0, max: 1, noNaN: true }),
  affinity: fc.double({ min: -1, max: 1, noNaN: true }),
  trust: fc.double({ min: 0, max: 1, noNaN: true }),
});

const FAMILIARITY_PHRASES = ["почти незнаком", "уже неплохо знаком", "хорошо знаешь"] as const;
const AFFINITY_PLUS = "он тебе симпатичен";
const AFFINITY_MINUS = "он тебе неприятен";
const TRUST_HIGH = "доверяешь ему";
const TRUST_LOW = "доверия мало";

function makeRelationship(values: { familiarity: number; affinity: number; trust: number }): Relationship {
  return {
    personEntityId: "1",
    scopeId: "chat:1",
    familiarity: values.familiarity,
    affinity: values.affinity,
    trust: values.trust,
    updatedAt: new Date().toISOString(),
  };
}

describe("PBT: describeRelationship — пороги категорий (§7.3)", () => {
  it("ровно одна фраза знакомства, и её уровень монотонен по familiarity", async () => {
    await fc.assert(
      fc.property(relationshipArb, relationshipArb, (a, b) => {
        const textA = describeRelationship(makeRelationship(a));
        const textB = describeRelationship(makeRelationship(b));
        const rankA = FAMILIARITY_PHRASES.findIndex((phrase) => textA.includes(phrase));
        const rankB = FAMILIARITY_PHRASES.findIndex((phrase) => textB.includes(phrase));
        expect(rankA).toBeGreaterThanOrEqual(0);
        expect(rankB).toBeGreaterThanOrEqual(0);
        // более высокая familiarity — не более низкий уровень фразы
        if (a.familiarity <= b.familiarity) expect(rankA).toBeLessThanOrEqual(rankB);
      }),
      { numRuns: 500 },
    );
  });

  it("фраза знакомства определяется порогами 0.2/0.6 (строгие нижние границы)", async () => {
    await fc.assert(
      fc.property(fc.double({ min: 0, max: 1, noNaN: true }), (familiarity) => {
        const text = describeRelationship(makeRelationship({ familiarity, affinity: 0, trust: 0.5 }));
        const expected =
          familiarity < 0.2
            ? FAMILIARITY_PHRASES[0]
            : familiarity < 0.6
              ? FAMILIARITY_PHRASES[1]
              : FAMILIARITY_PHRASES[2];
        expect(text).toBe(expected); // нейтральные affinity/trust ничего не добавляют
      }),
      { numRuns: 500 },
    );
  });

  it("симпатия/антипатия — только за строгими порогами ±0.3, одновременно не встречаются", async () => {
    await fc.assert(
      fc.property(fc.double({ min: -1, max: 1, noNaN: true }), (affinity) => {
        const text = describeRelationship(makeRelationship({ familiarity: 0.4, affinity, trust: 0.5 }));
        const hasPlus = text.includes(AFFINITY_PLUS);
        const hasMinus = text.includes(AFFINITY_MINUS);
        expect(hasPlus && hasMinus).toBe(false);
        expect(hasPlus).toBe(affinity > 0.3);
        expect(hasMinus).toBe(affinity < -0.3);
      }),
      { numRuns: 500 },
    );
  });

  it("доверие: строгие пороги 0.15 (мало) и 0.6 (высоко)", async () => {
    await fc.assert(
      fc.property(fc.double({ min: 0, max: 1, noNaN: true }), (trust) => {
        const text = describeRelationship(makeRelationship({ familiarity: 0.4, affinity: 0, trust }));
        expect(text.includes(TRUST_HIGH)).toBe(trust > 0.6);
        expect(text.includes(TRUST_LOW)).toBe(trust < 0.15);
      }),
      { numRuns: 500 },
    );
  });

  it("детерминированность: одинаковые числа — одинаковый текст", async () => {
    await fc.assert(
      fc.property(relationshipArb, (values) => {
        const one = describeRelationship(makeRelationship(values));
        const two = describeRelationship(makeRelationship(values));
        expect(one).toBe(two);
      }),
      { numRuns: 200 },
    );
  });

  it("граничные значения порогов (строгость сравнений) закреплены", () => {
    const at = (familiarity: number, affinity: number, trust: number) =>
      describeRelationship(makeRelationship({ familiarity, affinity, trust }));
    expect(at(0.2, 0, 0.5)).toBe("уже неплохо знаком"); // ровно 0.2 — уже вторая ступень
    expect(at(0.6, 0, 0.5)).toBe("хорошо знаешь");
    expect(at(0.4, 0.3, 0.5)).toBe("уже неплохо знаком"); // ровно 0.3 — ещё без симпатии
    expect(at(0.4, -0.3, 0.5)).toBe("уже неплохо знаком");
    expect(at(0.4, 0, 0.6)).toBe("уже неплохо знаком"); // ровно 0.6 — ещё без «доверяешь»
    expect(at(0.4, 0, 0.15)).toBe("уже неплохо знаком"); // ровно 0.15 — ещё не «мало»
    expect(at(0.4, 0.31, 0.61)).toBe("уже неплохо знаком, он тебе симпатичен, доверяешь ему");
  });
});
