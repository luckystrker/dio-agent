import { describe, expect, it } from "vitest";
import { TurnResultSchema } from "./turn-result.js";

// Модель иногда ставит confidence всем кандидатам памяти, а не только claim.
// Это не повод терять весь ход: лишнее поле у не-claim снимается.

describe("TurnResultSchema: confidence в кандидатах памяти", () => {
  it("снимает confidence у не-claim и сохраняет у claim", () => {
    const parsed = TurnResultSchema.parse({
      decision: "answer",
      messages: ["ок"],
      memory: [
        { kind: "preference", text: "любит чай", confidence: 0.9 },
        { kind: "claim", text: "живёт в Новосибирске", confidence: 0.8 },
      ],
    });

    expect(parsed.memory[0]).toEqual({
      kind: "preference",
      text: "любит чай",
      entities: [],
      salience: 0.5,
    });
    expect(parsed.memory[1]?.["confidence"]).toBe(0.8);
  });

  it("не трогает кандидата без confidence", () => {
    const parsed = TurnResultSchema.parse({
      decision: "answer",
      messages: ["ок"],
      memory: [{ kind: "thought", text: "что-то подумал" }],
    });
    expect(parsed.memory[0]).toEqual({
      kind: "thought",
      text: "что-то подумал",
      entities: [],
      salience: 0.5,
    });
  });
});
