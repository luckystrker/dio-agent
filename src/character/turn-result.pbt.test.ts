import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { TurnResultSchema, TURN_RESULT_JSON_SCHEMA } from "./turn-result.js";

// PBT согласованности двух схем одного контракта (§11.2, §17): JSON Schema
// уходит провайдеру в forced tool-calling, zod валидирует ответ до применения.
// Генератор строит объекты, валидные по СПЕЦИФИКАЦИИ (кросс-поля: answer
// требует 1–4 сообщения, ignore/defer — без сообщений), и такие объекты
// обязаны проходить обе схемы. Нарушения кросс-полей и лимитов — строгое
// отклонение, без «лучшего усилия».

const DECISIONS = ["answer", "ignore", "defer"] as const;
type Decision = (typeof DECISIONS)[number];

const decisionArb = fc.constantFrom(...DECISIONS);

const nonEmptyStringArb = fc.string({ minLength: 1, maxLength: 200, unit: "grapheme" });

interface TurnResultCandidate {
  decision: Decision;
  messages: string[];
  notes: string[];
  reason?: string | undefined;
}

/** Спецификационно-валидный кандидат: кросс-поля §11.2 + лимиты обеих схем. */
const specValidArb: fc.Arbitrary<TurnResultCandidate> = fc
  .record({
    decision: decisionArb,
    answerMessages: fc.array(nonEmptyStringArb, { minLength: 1, maxLength: 4 }),
    silentMessages: fc.constant([] as string[]),
    notes: fc.array(nonEmptyStringArb, { maxLength: 5 }),
    reason: fc.option(fc.string({ minLength: 1, maxLength: 500, unit: "grapheme" }), {
      nil: undefined,
    }),
  })
  .map(({ decision, answerMessages, silentMessages, notes, reason }) => ({
    decision,
    messages: decision === "answer" ? answerMessages : silentMessages,
    notes,
    reason,
  }));

describe("PBT: TurnResultSchema принимает всё, что требует спецификация", () => {
  it("любой спека-валидный TurnResult проходит zod без изменений полей", async () => {
    await fc.assert(
      fc.property(specValidArb, (candidate) => {
        const parsed = TurnResultSchema.safeParse(candidate);
        expect(parsed.success).toBe(true);
        if (parsed.success) {
          expect(parsed.data.decision).toBe(candidate.decision);
          expect(parsed.data.messages).toEqual(candidate.messages);
          expect(parsed.data.notes).toEqual(candidate.notes);
          expect(parsed.data.reason).toBe(candidate.reason ?? undefined);
        }
      }),
    );
  });

  it("минимальный объект {decision: ignore|defer} разбирается с дефолтами", async () => {
    await fc.assert(
      fc.property(fc.constantFrom("ignore", "defer" as const), (decision) => {
        const parsed = TurnResultSchema.parse({ decision });
        expect(parsed.messages).toEqual([]);
        expect(parsed.notes).toEqual([]);
        expect(parsed.reason).toBeUndefined();
      }),
    );
  });
});

describe("PBT: кросс-поля §11.2 строго валидируются", () => {
  it("answer с 0 сообщений отвергается на любом наборе прочих полей", async () => {
    await fc.assert(
      fc.property(
        fc.array(nonEmptyStringArb, { maxLength: 5 }),
        fc.option(fc.string({ minLength: 1, maxLength: 500, unit: "grapheme" }), { nil: undefined }),
        (notes, reason) => {
          const result = TurnResultSchema.safeParse({ decision: "answer", notes, reason });
          expect(result.success).toBe(false);
          if (!result.success) {
            expect(JSON.stringify(result.error.issues)).toContain("messages");
          }
        },
      ),
    );
  });

  it("ignore/defer с любым непустым списком сообщений отвергаются", async () => {
    const nonEmptyMessagesArb = fc.array(nonEmptyStringArb, { minLength: 1, maxLength: 4 });
    await fc.assert(
      fc.property(
        fc.constantFrom("ignore", "defer" as const),
        nonEmptyMessagesArb,
        (decision, messages) => {
          expect(TurnResultSchema.safeParse({ decision, messages }).success).toBe(false);
        },
      ),
    );
  });

  it("границы: ровно 1 и ровно 4 сообщения при answer валидны, 5 — нет", () => {
    expect(TurnResultSchema.safeParse({ decision: "answer", messages: ["один"] }).success).toBe(true);
    expect(
      TurnResultSchema.safeParse({ decision: "answer", messages: ["1", "2", "3", "4"] }).success,
    ).toBe(true);
    expect(
      TurnResultSchema.safeParse({ decision: "answer", messages: ["1", "2", "3", "4", "5"] }).success,
    ).toBe(false);
  });
});

describe("PBT: строгие отклонения схемы (§17: невалидное не применяется)", () => {
  it("decision вне перечисления отвергается", async () => {
    const badDecisionArb = fc
      .string({ minLength: 1, maxLength: 20, unit: "grapheme" })
      .filter((value) => !DECISIONS.includes(value as Decision));
    await fc.assert(
      fc.property(badDecisionArb, (decision) => {
        expect(TurnResultSchema.safeParse({ decision }).success).toBe(false);
      }),
    );
  });

  it("отсутствие decision отвергается", async () => {
    const noDecisionArb = fc.record({
      notes: fc.array(nonEmptyStringArb, { maxLength: 5 }),
      reason: fc.option(nonEmptyStringArb, { nil: undefined }),
    });
    await fc.assert(
      fc.property(noDecisionArb, (candidate) => {
        expect(TurnResultSchema.safeParse(candidate).success).toBe(false);
      }),
    );
  });

  it("лишние ключи отвергаются (.strict): контракт для модели и runtime один", async () => {
    const extraKeyArb = fc
      .string({ minLength: 1, maxLength: 10, unit: "grapheme" })
      .filter((key) => !["decision", "messages", "notes", "reason"].includes(key));
    const extraValueArb = fc.jsonValue({ maxDepth: 2 });
    await fc.assert(
      fc.property(specValidArb, extraKeyArb, extraValueArb, (candidate, key, value) => {
        expect(
          TurnResultSchema.safeParse({ ...candidate, [key]: value }).success,
        ).toBe(false);
      }),
    );
  });

  it("пустые строки в messages/notes и reason длиннее 500 отвергаются", () => {
    expect(TurnResultSchema.safeParse({ decision: "answer", messages: [""] }).success).toBe(false);
    expect(TurnResultSchema.safeParse({ decision: "ignore", notes: [""] }).success).toBe(false);
    expect(
      TurnResultSchema.safeParse({ decision: "ignore", reason: "р".repeat(501) }).success,
    ).toBe(false);
    // Границы: ровно 500 — валидно.
    expect(TurnResultSchema.safeParse({ decision: "ignore", reason: "р".repeat(500) }).success).toBe(
      true,
    );
  });

  it("больше 5 notes отвергается (граница maxItems)", async () => {
    const oversizedArb = fc.integer({ min: 6, max: 20 }).map((n) =>
      Array.from({ length: n }, (_unused, i) => `note-${i}`),
    );
    await fc.assert(
      fc.property(decisionArb, oversizedArb, (decision, notes) => {
        expect(TurnResultSchema.safeParse({ decision, notes }).success).toBe(false);
      }),
    );
  });
});

describe("согласованность TURN_RESULT_JSON_SCHEMA с zod-схемой", () => {
  it("перечисление решений совпадает с zod-перечислением", () => {
    expect([...TURN_RESULT_JSON_SCHEMA.properties.decision.enum].sort()).toEqual(
      [...DECISIONS].sort(),
    );
  });

  it("набор свойств и required совпадают с zod-схемой", () => {
    expect(Object.keys(TURN_RESULT_JSON_SCHEMA.properties).sort()).toEqual(
      ["decision", "memory", "messages", "notes", "reason"],
    );
    expect([...TURN_RESULT_JSON_SCHEMA.required]).toEqual(["decision"]);
  });

  it("лимиты JSON Schema зеркалируют zod: maxItems 4/5, minLength 1, reason 500", () => {
    expect(TURN_RESULT_JSON_SCHEMA.properties.messages.maxItems).toBe(4);
    expect(TURN_RESULT_JSON_SCHEMA.properties.messages.items.minLength).toBe(1);
    expect(TURN_RESULT_JSON_SCHEMA.properties.notes.maxItems).toBe(5);
    expect(TURN_RESULT_JSON_SCHEMA.properties.notes.items.minLength).toBe(1);
    expect(TURN_RESULT_JSON_SCHEMA.properties.reason.maxLength).toBe(500);
    expect(TURN_RESULT_JSON_SCHEMA.additionalProperties).toBe(false);
  });

  it("поведенческая сверка: JSON-Schema-допустимые лимиты и zod совпадают на границах", () => {
    // 4 сообщения + answer — допустимо обеим схемам; 5 — отвергается zod.
    expect(TurnResultSchema.safeParse({ decision: "answer", messages: ["1", "2", "3", "4"] }).success).toBe(true);
    expect(TurnResultSchema.safeParse({ decision: "answer", messages: ["1", "2", "3", "4", "5"] }).success).toBe(false);
    // reason 500 — допустимо; 501 — отвергается (maxLength JSON Schema).
    expect(TurnResultSchema.safeParse({ decision: "ignore", reason: "x".repeat(500) }).success).toBe(true);
    expect(TurnResultSchema.safeParse({ decision: "ignore", reason: "x".repeat(501) }).success).toBe(false);
  });

  it("кросс-поля §11.2 живут в zod (JSON Schema их не выражает) — обе стороны контракта", async () => {
    // Провайдер получает JSON Schema без кросс-полей; runtime обязан
    // добивать их сам — проверяем, что zod действительно отвергает.
    const jsonSchemaAllows = (value: unknown): boolean => {
      const record = value as Record<string, unknown>;
      if (typeof record !== "object" || record === null) return false;
      if (!DECISIONS.includes(record["decision"] as Decision)) return false;
      for (const key of Object.keys(record)) {
        if (!["decision", "messages", "notes", "reason"].includes(key)) return false;
      }
      const messages = record["messages"] as string[] | undefined ?? [];
      if (messages.length > 4 || messages.some((m) => m.length === 0)) return false;
      const notes = record["notes"] as string[] | undefined ?? [];
      if (notes.length > 5 || notes.some((n) => n.length === 0)) return false;
      const reason = record["reason"] as string | undefined;
      if (reason !== undefined && reason.length > 500) return false;
      return true;
    };
    // Объект, проходящий JSON Schema, но нарушающий кросс-поля, — zod обязан отвергнуть.
    expect(jsonSchemaAllows({ decision: "ignore", messages: ["лишнее"] })).toBe(true);
    expect(TurnResultSchema.safeParse({ decision: "ignore", messages: ["лишнее"] }).success).toBe(false);
    expect(jsonSchemaAllows({ decision: "answer", messages: [] })).toBe(true);
    expect(TurnResultSchema.safeParse({ decision: "answer", messages: [] }).success).toBe(false);
  });
});
