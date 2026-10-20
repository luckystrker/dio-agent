import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { DeltaError, validateDeltas, type RuntimeDelta } from "./deltas.js";

// PBT по спецификации «дельты валидируются до применения» (§17, инвариант 6):
// валидное проходит, невалидное (тип/текст) — DeltaError, без «лучшего усилия».

const MAX_TEXT_LENGTH = 2000;

/** Строка, удовлетворяющая контракту text: непустая и не длиннее 2000. */
const validTextArb = fc.string({ minLength: 1, maxLength: MAX_TEXT_LENGTH, unit: "grapheme" });

const validDeltaArb = fc.record({
  type: fc.constant("append_note"),
  text: validTextArb,
});

const validListArb = fc.array(validDeltaArb, { maxLength: 20 });

describe("PBT: validateDeltas — валидные списки", () => {
  it("любой сгенерированный валидный список проходит и сохраняется как есть", async () => {
    await fc.assert(
      fc.property(validListArb, (list) => {
        const result = validateDeltas(list);
        expect(result).toEqual(list);
      }),
    );
  });

  it("длина результата совпадает с длиной входа (ничего не теряется и не добавляется)", async () => {
    await fc.assert(
      fc.property(validListArb, (list) => {
        expect(validateDeltas(list)).toHaveLength(list.length);
      }),
    );
  });

  it("раунд-трип: JSON → validateDeltas → JSON → validateDeltas стабилен", async () => {
    await fc.assert(
      fc.property(validListArb, (list) => {
        const once = validateDeltas(list);
        const viaJson = JSON.parse(JSON.stringify(once)) as unknown;
        expect(validateDeltas(viaJson)).toEqual(once);
      }),
    );
  });

  it("граница длины: ровно 2000 символов валидно (верхняя допустимая граница)", () => {
    expect(validateDeltas([{ type: "append_note", text: "а".repeat(MAX_TEXT_LENGTH) }])).toHaveLength(1);
  });
});

describe("PBT: validateDeltas — отклонения", () => {
  it("любая дельта неизвестного типа отклоняется с DeltaError", async () => {
    const nonAppendNoteArb = fc
      .string({ minLength: 0, maxLength: 30, unit: "grapheme" })
      .filter((value) => value !== "append_note");
    await fc.assert(
      fc.property(validListArb, nonAppendNoteArb, fc.nat(), (list, badType, position) => {
        const badDelta = { type: badType, text: "текст" };
        const index = position % (list.length + 1);
        const input = [...list.slice(0, index), badDelta, ...list.slice(index)];
        expect(() => validateDeltas(input)).toThrow(DeltaError);
      }),
    );
  });

  it("дельта без поля type отклоняется с DeltaError", async () => {
    await fc.assert(
      fc.property(validTextArb, fc.nat(), (text, position) => {
        const list = [
          { type: "append_note", text: "ок" },
          { type: "append_note", text },
        ];
        const index = position % (list.length + 1);
        const input = [...list.slice(0, index), { text }, ...list.slice(index)];
        expect(() => validateDeltas(input)).toThrow(DeltaError);
      }),
    );
  });

  it("пустой text отклоняется на любой позиции списка", async () => {
    await fc.assert(
      fc.property(validListArb, fc.nat(), (list, position) => {
        const index = position % (list.length + 1);
        const input = [
          ...list.slice(0, index),
          { type: "append_note", text: "" },
          ...list.slice(index),
        ];
        expect(() => validateDeltas(input)).toThrow(DeltaError);
      }),
    );
  });

  it("перегруженный text (длиннее 2000) отклоняется на любой позиции", async () => {
    const tooLongArb = fc.integer({ min: MAX_TEXT_LENGTH + 1, max: MAX_TEXT_LENGTH * 3 });
    await fc.assert(
      fc.property(validListArb, tooLongArb, fc.nat(), (list, length, position) => {
        const index = position % (list.length + 1);
        const input = [
          ...list.slice(0, index),
          { type: "append_note", text: "а".repeat(length) },
          ...list.slice(index),
        ];
        expect(() => validateDeltas(input)).toThrow(DeltaError);
      }),
    );
  });

  it("не-массив (произвольный JSON) отклоняется с DeltaError", async () => {
    const notArrayArb = fc
      .jsonValue({ maxDepth: 3 })
      .filter((value) => !Array.isArray(value));
    await fc.assert(
      fc.property(notArrayArb, (value) => {
        expect(() => validateDeltas(value)).toThrow(DeltaError);
      }),
    );
  });
});

describe("PBT: validateDeltas — нормализация результата", () => {
  it("лишние поля дельты отбрасываются: результат содержит только type и text", async () => {
    const extraKeysArb = fc.dictionary(
      fc.string({ minLength: 1, maxLength: 8, unit: "grapheme" }).filter((k) => k !== "type" && k !== "text"),
      fc.jsonValue({ maxDepth: 2 }),
    );
    await fc.assert(
      fc.property(validDeltaArb, extraKeysArb, (delta, extra) => {
        const result = validateDeltas([{ ...delta, ...extra }]);
        expect(result).toEqual([{ type: delta.type, text: delta.text }] satisfies RuntimeDelta[]);
      }),
    );
  });
});
