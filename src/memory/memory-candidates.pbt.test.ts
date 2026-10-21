import { describe, expect, it } from "vitest";
import fc from "fast-check";
import {
  CandidateError,
  MAX_ENTITIES_PER_CANDIDATE,
  MAX_MEMORY_CANDIDATES,
  MEMORY_KINDS,
  validateMemoryCandidates,
} from "./memory-candidates.js";
import { ENTITY_KINDS } from "./entities.js";

// PBT валидации кандидатов памяти (§10.1, §8.2): runtime проверяет типы,
// длины, лимиты, допустимые значения. Границы проверяются точно: 5/6
// кандидатов, 2000/2001 символ, 3/4 сущности, salience/confidence 0..1.

const NON_CLAIM_KINDS = ["experience", "thought", "preference"] as const;
const ENTITY_ROLES = ["subject", "participant", "place", "context"] as const;

// Посторонние ключи (§8.3/§10.1): провенанс и ID модель предлагать не может.
// Отклонение делает именно валидатор — до какой-либо zod-схемы адаптера.
const ALLOWED_CANDIDATE_KEYS = ["kind", "text", "entities", "salience", "confidence"] as const;
const foreignKeyArb = fc
  .string({ minLength: 1, maxLength: 16, unit: "grapheme" })
  .filter((key) => !(ALLOWED_CANDIDATE_KEYS as readonly string[]).includes(key));

const kindArb = fc.constantFrom(...MEMORY_KINDS);
const nonClaimKindArb = fc.constantFrom(...NON_CLAIM_KINDS);

// Алфавит из одиночных UTF-16 юнитов (кириллица BMP): длина строки в
// символах совпадает с .length — «валидный по спеке» генератор не должен
// спотыкаться о подсчёт длины в юнитах.
const ALPHABET = "абвгдежзийклмнопрстуфхцчшщъыьэюя ABC123.,!?-".split("");

function alphabetString(maxLength: number): fc.Arbitrary<string> {
  return fc
    .array(fc.constantFrom(...ALPHABET), { minLength: 1, maxLength })
    .map((chars) => chars.join(""))
    .filter((value) => value.trim().length > 0);
}

const textArb = alphabetString(2000);
const entityNameArb = alphabetString(200);

const entityArb = fc.record({
  name: entityNameArb,
  kind: fc.option(fc.constantFrom(...ENTITY_KINDS), { nil: undefined }),
  role: fc.option(fc.constantFrom(...ENTITY_ROLES), { nil: undefined }),
});

const unitIntervalArb = fc.double({ min: 0, max: 1, noNaN: true });

/** Спецификационно-валидный кандидат (confidence — только у claim, §8.2). */
const validCandidateArb = fc
  .record({
    kind: kindArb,
    text: textArb,
    entities: fc.array(entityArb, { maxLength: MAX_ENTITIES_PER_CANDIDATE }),
    salience: fc.option(unitIntervalArb, { nil: undefined }),
    confidence: fc.option(unitIntervalArb, { nil: undefined }),
  })
  .map((candidate) => ({
    ...candidate,
    // у непроверяемых видов confidence отсутствует вовсе
    confidence:
      candidate.kind === "claim" ? candidate.confidence : (undefined as number | undefined),
  }));

describe("PBT: валидный кандидат проходит валидацию и нормализуется", () => {
  it("1..5 валидных кандидатов принимаются; поля сохраняются, дефолты проставляются", async () => {
    await fc.assert(
      fc.property(
        fc.array(validCandidateArb, { minLength: 1, maxLength: MAX_MEMORY_CANDIDATES }),
        (input) => {
          const result = validateMemoryCandidates(input);
          expect(result).toHaveLength(input.length);
          result.forEach((candidate, index) => {
            const source = input[index]!;
            expect(candidate.kind).toBe(source.kind);
            expect(candidate.text).toBe(source.text.trim());
            expect(candidate.salience).toBe(source.salience ?? 0.5);
            expect(candidate.confidence).toBe(
              source.kind === "claim" ? (source.confidence ?? undefined) : undefined,
            );
            expect(candidate.entities).toHaveLength(source.entities.length);
            source.entities.forEach((entity, entityIndex) => {
              expect(candidate.entities[entityIndex]).toEqual({
                name: entity.name.trim(),
                kind: entity.kind ?? "topic",
                role: entity.role ?? "subject",
              });
            });
          });
        },
      ),
    );
  });

  it("текст нормализуется: краёвные пробелы обрезаются", async () => {
    await fc.assert(
      fc.property(alphabetString(500), (raw) => {
        // ядро без своих краёв-пробелов — чтобы сравнение было точным
        const core = raw.replaceAll(/^ +| +$/g, "");
        expect(core.length).toBeGreaterThan(0);
        const [candidate] = validateMemoryCandidates([{ kind: "claim", text: `  ${core}  ` }]);
        expect(candidate!.text).toBe(core);
      }),
    );
  });
});

describe("PBT: границы массивов и длин (§10.1)", () => {
  it("ровно 5 кандидатов валидны, 6 — весь набор отклонён", () => {
    const five = Array.from({ length: 5 }, (_unused, i) => ({
      kind: "experience" as const,
      text: `событие ${i}`,
    }));
    expect(validateMemoryCandidates(five)).toHaveLength(5);
    const six = [...five, { kind: "experience" as const, text: "шестое событие" }];
    expect(() => validateMemoryCandidates(six)).toThrow(CandidateError);
  });

  it("для любой длины текста 1..2000 валиден, 2001+ — отклонён", async () => {
    await fc.assert(
      fc.property(
        fc.integer({ min: 1, max: 2100 }),
        fc.constantFrom(...MEMORY_KINDS),
        (length, kind) => {
          const text = "а".repeat(length);
          if (length <= 2000) {
            expect(() => validateMemoryCandidates([{ kind, text }])).not.toThrow();
          } else {
            expect(() => validateMemoryCandidates([{ kind, text }])).toThrow(/длиннее 2000/);
          }
        },
      ),
    );
  });

  it("граница текста точно 2000/2001 символ", () => {
    expect(() =>
      validateMemoryCandidates([{ kind: "claim", text: "а".repeat(2000) }]),
    ).not.toThrow();
    expect(() => validateMemoryCandidates([{ kind: "claim", text: "а".repeat(2001) }])).toThrow(
      CandidateError,
    );
  });

  it("3 сущности валидны, 4 — отклонены; ровно на границе", () => {
    const make = (count: number) =>
      Array.from({ length: count }, (_unused, i) => ({ name: `сущность ${i}` }));
    expect(() =>
      validateMemoryCandidates([{ kind: "claim", text: "текст", entities: make(3) }]),
    ).not.toThrow();
    expect(() =>
      validateMemoryCandidates([{ kind: "claim", text: "текст", entities: make(4) }]),
    ).toThrow(/entities/);
  });
});

describe("PBT: salience/confidence — числа 0..1 (§8.2)", () => {
  it("0 и 1 включительно валидны на точных границах", () => {
    for (const value of [0, 1]) {
      expect(() =>
        validateMemoryCandidates([
          { kind: "claim", text: "текст", salience: value, confidence: value },
        ]),
      ).not.toThrow();
    }
  });

  it("salience вне [0,1] отклонён на любом валидном кандидате", async () => {
    const outsideArb = fc.oneof(
      fc.double({ min: -5, max: -Number.MIN_VALUE, noNaN: true }),
      fc.double({ min: 1.0000001, max: 5, noNaN: true }),
    );
    await fc.assert(
      fc.property(validCandidateArb, outsideArb, (candidate, salience) => {
        expect(() =>
          validateMemoryCandidates([{ ...candidate, salience }]),
        ).toThrow(/salience/);
      }),
    );
  });

  it("confidence вне [0,1] отклонён у claim", async () => {
    const outsideArb = fc.oneof(
      fc.double({ min: -5, max: -Number.MIN_VALUE, noNaN: true }),
      fc.double({ min: 1.0000001, max: 5, noNaN: true }),
    );
    await fc.assert(
      fc.property(outsideArb, (confidence) => {
        expect(() =>
          validateMemoryCandidates([{ kind: "claim", text: "текст", confidence }]),
        ).toThrow(/confidence/);
      }),
    );
  });

  it("confidence допустим ТОЛЬКО у claim (§8.2: у предпочтения нет истинности)", async () => {
    await fc.assert(
      fc.property(nonClaimKindArb, unitIntervalArb, (kind, confidence) => {
        expect(() =>
          validateMemoryCandidates([{ kind, text: "текст", confidence }]),
        ).toThrow(/confidence.*claim/);
      }),
    );
    await fc.assert(
      fc.property(kindArb, (kind) => {
        expect(() =>
          validateMemoryCandidates([{ kind, text: "текст" }]),
        ).not.toThrow();
      }),
    );
  });
});

describe("PBT: енумы kinds/roles строго проверяются", () => {
  const badKindArb = fc
    .string({ minLength: 1, maxLength: 12, unit: "grapheme" })
    .filter((value) => !(MEMORY_KINDS as readonly string[]).includes(value));
  const badEntityKindArb = fc
    .string({ minLength: 1, maxLength: 12, unit: "grapheme" })
    .filter((value) => !(ENTITY_KINDS as readonly string[]).includes(value));
  const badRoleArb = fc
    .string({ minLength: 1, maxLength: 12, unit: "grapheme" })
    .filter((value) => !(ENTITY_ROLES as readonly string[]).includes(value));

  it("любой вид вне перечисления отклонён; все виды перечисления приняты", async () => {
    await fc.assert(
      fc.property(validCandidateArb, badKindArb, (candidate, kind) => {
        expect(() => validateMemoryCandidates([{ ...candidate, kind }])).toThrow(CandidateError);
      }),
    );
  });

  it("вид сущности и роль вне перечислений отклонены", async () => {
    await fc.assert(
      fc.property(
        badEntityKindArb,
        badRoleArb,
        fc.constantFrom(...ENTITY_ROLES),
        fc.constantFrom(...ENTITY_KINDS),
        (badKind, badRole, goodRole, goodKind) => {
          expect(() =>
            validateMemoryCandidates([
              { kind: "claim", text: "текст", entities: [{ name: "имя", kind: badKind, role: goodRole }] },
            ]),
          ).toThrow(/kind сущности/);
          expect(() =>
            validateMemoryCandidates([
              { kind: "claim", text: "текст", entities: [{ name: "имя", kind: goodKind, role: badRole }] },
            ]),
          ).toThrow(/роль/);
        },
      ),
    );
  });
});

describe("PBT: посторонние поля отклоняются валидатором (§10.1)", () => {
  it("любой посторонний ключ на валидном кандидате → CandidateError «постороннее поле»", async () => {
    await fc.assert(
      fc.property(
        validCandidateArb,
        foreignKeyArb,
        fc.constantFrom("x", 1, null, true, { nested: true }),
        (candidate, key, value) => {
          expect(() => validateMemoryCandidates([{ ...candidate, [key]: value }])).toThrow(
            /постороннее поле/,
          );
        },
      ),
    );
  });
});

describe("PBT: сбой одного кандидата валит весь набор (нет «лучшего усилия», §10.1)", () => {
  it("один невалидный среди валидных — весь вход отклонён", async () => {
    const poisonArb = badPoisonArb();
    await fc.assert(
      fc.property(
        fc.array(validCandidateArb, { minLength: 0, maxLength: 4 }),
        poisonArb,
        (valid, poison) => {
          const middle = Math.floor(valid.length / 2);
          const input = [...valid.slice(0, middle), poison, ...valid.slice(middle)];
          expect(() => validateMemoryCandidates(input)).toThrow(CandidateError);
        },
      ),
    );
  });
});

/** Невалидные по спеке кандидаты: битый вид, пустой/длинный текст, мусорные числа, посторонние поля. */
function badPoisonArb(): fc.Arbitrary<Record<string, unknown>> {
  return fc.oneof(
    fc.record({ kind: fc.constant("vibe"), text: fc.constant("текст") }),
    fc.record({ kind: kindArb, text: fc.constant("   ") }),
    fc.record({ kind: kindArb, text: fc.constant("а".repeat(2001)) }),
    fc.record({
      kind: kindArb,
      text: textArb,
      salience: fc.double({ min: -1, max: -0.0001, noNaN: true }),
    }),
    fc.record({
      kind: nonClaimKindArb,
      text: textArb,
      confidence: fc.double({ min: 0, max: 1, noNaN: true }),
    }),
    fc.record({
      kind: kindArb,
      text: textArb,
      entities: fc.constant(Array.from({ length: 4 }, (_unused, i) => ({ name: `имя ${i}` }))),
    }),
    // валидный кандидат + случайный посторонний ключ (source/id/…)
    fc
      .tuple(validCandidateArb, foreignKeyArb)
      .map(([candidate, key]) => ({ ...candidate, [key]: "poison" }) as Record<string, unknown>),
  );
}
