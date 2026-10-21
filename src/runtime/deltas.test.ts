import { describe, expect, it } from "vitest";
import { DeltaError, validateDeltas } from "./deltas.js";

describe("validateDeltas", () => {
  it("принимает корректный список append_note", () => {
    expect(validateDeltas([{ type: "append_note", text: "привет" }])).toEqual([
      { type: "append_note", text: "привет" },
    ]);
  });

  it("пустой список валиден (нет изменений — тоже результат)", () => {
    expect(validateDeltas([])).toEqual([]);
  });

  it("не-массив отклоняется", () => {
    expect(() => validateDeltas({})).toThrow(DeltaError);
    expect(() => validateDeltas(null)).toThrow(DeltaError);
  });

  it("неизвестный тип дельты отклоняется с индексом", () => {
    expect(() =>
      validateDeltas([{ type: "append_note", text: "ок" }, { type: "delete_all", text: "зло" }]),
    ).toThrow(/#1.*delete_all/);
  });

  it("элемент не-объект (число/строка/null) отклоняется именно как «ожидается объект с полем type»", () => {
    // отдельные ветки guard'а §12.1: typeof, null, отсутствие поля type
    expect(() => validateDeltas([5])).toThrow(/#0.*ожидается объект с полем type/);
    expect(() => validateDeltas(["запись"])).toThrow(/#0.*ожидается объект с полем type/);
    expect(() => validateDeltas([null])).toThrow(/#0.*ожидается объект с полем type/);
    expect(() => validateDeltas([{}])).toThrow(/#0.*ожидается объект с полем type/);
  });

  it("пустой текст отклоняется", () => {
    expect(() => validateDeltas([{ type: "append_note", text: "" }])).toThrow(DeltaError);
  });

  it("текст длиннее 2000 символов отклоняется", () => {
    expect(() => validateDeltas([{ type: "append_note", text: "а".repeat(2001) }])).toThrow(DeltaError);
  });
});
