import { describe, expect, it } from "vitest";
import {
  CandidateError,
  MAX_MEMORY_CANDIDATES,
  validateMemoryCandidates,
} from "./memory-candidates.js";

const VALID = {
  kind: "claim",
  text: "Пользователь сказал, что любит горы",
  entities: [{ name: "горы", kind: "topic", role: "subject" }],
  salience: 0.7,
  confidence: 0.6,
};

describe("validateMemoryCandidates", () => {
  it("принимает валидного кандидата и проставляет дефолты", () => {
    const [candidate] = validateMemoryCandidates([
      { kind: "experience", text: "Обсуждали отпуск" },
    ]);
    expect(candidate).toMatchObject({
      kind: "experience",
      text: "Обсуждали отпуск",
      salience: 0.5,
      entities: [],
    });
  });

  it("пустой список валиден", () => {
    expect(validateMemoryCandidates([])).toEqual([]);
  });

  it("не-массив и больше 5 кандидатов отклоняются", () => {
    expect(() => validateMemoryCandidates({})).toThrow(CandidateError);
    const six = Array.from({ length: MAX_MEMORY_CANDIDATES + 1 }, () => ({ ...VALID }));
    expect(() => validateMemoryCandidates(six)).toThrow(/5 кандидатов/);
  });

  it("неизвестный вид отклоняется", () => {
    expect(() => validateMemoryCandidates([{ ...VALID, kind: "vibe" }])).toThrow(/#0.*vibe/);
  });

  it("пустой и перегруженный текст отклоняются", () => {
    expect(() => validateMemoryCandidates([{ ...VALID, text: "" }])).toThrow(CandidateError);
    expect(() => validateMemoryCandidates([{ ...VALID, text: "а".repeat(2001) }])).toThrow(CandidateError);
  });

  it("confidence вне claim отклоняется (§8.2: у предпочтения нет истинности)", () => {
    expect(() =>
      validateMemoryCandidates([{ ...VALID, kind: "preference", confidence: 0.9 }]),
    ).toThrow(/confidence.*claim/);
    const { confidence: _omit, ...withoutConfidence } = VALID;
    expect(() =>
      validateMemoryCandidates([{ ...withoutConfidence, kind: "preference" }]),
    ).not.toThrow();
  });

  it("salience/confidence вне 0..1 отклоняются", () => {
    expect(() => validateMemoryCandidates([{ ...VALID, salience: 1.5 }])).toThrow(CandidateError);
    expect(() => validateMemoryCandidates([{ ...VALID, confidence: -0.1 }])).toThrow(CandidateError);
  });

  it("больше 3 сущностей у кандидата отклоняется", () => {
    const entities = Array.from({ length: 4 }, (_, i) => ({
      name: `сущность${i}`,
      kind: "topic",
      role: "subject",
    }));
    expect(() => validateMemoryCandidates([{ ...VALID, entities }])).toThrow(/entities/);
  });

  it("неизвестная роль/вид сущности отклоняются", () => {
    expect(() =>
      validateMemoryCandidates([{ ...VALID, entities: [{ name: "x", kind: "topic", role: "boss" }] }]),
    ).toThrow(CandidateError);
    expect(() =>
      validateMemoryCandidates([{ ...VALID, entities: [{ name: "x", kind: "dragon", role: "subject" }] }]),
    ).toThrow(CandidateError);
  });
});
