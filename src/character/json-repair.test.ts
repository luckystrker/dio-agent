import { describe, expect, it } from "vitest";
import { parseToolArguments } from "./json-repair.js";

// Ремонт почти-JSON из tool-call: модель ломает его регулярно, ход терять нельзя.

describe("parseToolArguments", () => {
  it("разбирает валидный JSON как есть", () => {
    expect(parseToolArguments('{"decision":"answer"}')).toEqual({ decision: "answer" });
  });

  it("чинит двойное двоеточие после ключа", () => {
    expect(parseToolArguments('{"mood_delta":: {"valence": 0.1}}')).toEqual({
      mood_delta: { valence: 0.1 },
    });
  });

  it("чинит оборванный массив через jsonrepair", () => {
    expect(parseToolArguments('{"messages": ["ок", "ещё"')).toEqual({
      messages: ["ок", "ещё"],
    });
  });

  it("чинит висячую запятую", () => {
    expect(parseToolArguments('{"messages": ["ок",]}')).toEqual({ messages: ["ок"] });
  });

  it("бросает на полном мусоре", () => {
    expect(() => parseToolArguments("не json вообще")).toThrow();
  });
});
