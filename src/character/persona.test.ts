import { describe, expect, it } from "vitest";
import { buildSystemPrompt, DEFAULT_PERSONA } from "./persona.js";
import { describeRelationship, RELATIONSHIP_LIMITS } from "./relationships.js";

const PERSONA = {
  id: "1",
  version: 1,
  name: DEFAULT_PERSONA.name,
  description: DEFAULT_PERSONA.description,
  speechStyle: DEFAULT_PERSONA.speechStyle,
  values: [...DEFAULT_PERSONA.values],
  moodBaseline: { ...DEFAULT_PERSONA.moodBaseline },
  seedBio: DEFAULT_PERSONA.seedBio,
};

describe("buildSystemPrompt (§7.1, §10.4)", () => {
  it("persona-блок: имя, манера, ценности, seed как вымысел", () => {
    const prompt = buildSystemPrompt(PERSONA);
    expect(prompt).toContain("Ты — Dio");
    expect(prompt).toContain("Манера речи");
    expect(prompt).toContain("вымысел");
  });

  it("экстры добавляются: настроение, отношения, память", () => {
    const prompt = buildSystemPrompt(PERSONA, {
      moodLine: "в приподнятом настроении",
      relationshipLine: "уже неплохо знаком",
      memoryBlock: "- (claim) любит горы",
    });
    expect(prompt).toContain("Твоё состояние сейчас: в приподнятом настроении");
    expect(prompt).toContain("Отношение к собеседнику: уже неплохо знаком");
    expect(prompt).toContain("любит горы");
  });

  it("без памяти блок памяти не добавляется", () => {
    expect(buildSystemPrompt(PERSONA)).not.toContain("долговременная память");
  });
});

describe("describeRelationship (§7.3)", () => {
  const base = { personEntityId: "1", scopeId: "chat:1", updatedAt: "2026-01-01" };

  it("незнаком и нейтрален — короткая строка", () => {
    expect(describeRelationship({ ...base, familiarity: 0, affinity: 0, trust: 0.2 })).toBe(
      "почти незнаком",
    );
  });

  it("симпатия и доверие отражаются", () => {
    const line = describeRelationship({ ...base, familiarity: 0.7, affinity: 0.5, trust: 0.8 });
    expect(line).toContain("хорошо знаешь");
    expect(line).toContain("симпатичен");
    expect(line).toContain("доверяешь");
  });

  it("неприятность отражается", () => {
    expect(
      describeRelationship({ ...base, familiarity: 0.3, affinity: -0.5, trust: 0.2 }),
    ).toContain("неприятен");
  });
});

describe("RELATIONSHIP_LIMITS", () => {
  it("лимиты на ход малы и положительны (§7.3)", () => {
    expect(RELATIONSHIP_LIMITS.affinityPerTurn).toBeLessThanOrEqual(0.1);
    expect(RELATIONSHIP_LIMITS.trustPerTurn).toBeLessThanOrEqual(0.1);
    expect(RELATIONSHIP_LIMITS.familiarityPerSignificantTurn).toBeGreaterThan(0);
  });
});
