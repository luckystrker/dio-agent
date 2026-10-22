import { describe, expect, it } from "vitest";
import { DEFAULT_PERSONA } from "./persona.js";
import { DEFAULT_SYSTEM_PROMPT_TEMPLATE, renderPersonaBlock, renderSystemPrompt } from "./prompt-template.js";

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

describe("renderSystemPrompt (§7.1: каркас промпта — редактируемый шаблон)", () => {
  it("метки заменяются на состояние: персона, настроение, отношения, память", () => {
    const prompt = renderSystemPrompt(DEFAULT_SYSTEM_PROMPT_TEMPLATE, PERSONA, {
      moodLine: "в приподнятом настроении",
      relationshipLine: "уже неплохо знаком",
      memoryBlock: "- (claim) любит горы",
    });
    expect(prompt).toContain("Ты — Dio");
    expect(prompt).toContain("Твоё состояние сейчас: в приподнятом настроении");
    expect(prompt).toContain("Отношение к собеседнику: уже неплохо знаком");
    expect(prompt).toContain("любит горы");
  });

  it("абзац с пустой меткой исчезает целиком — вместе с инструкцией вокруг неё", () => {
    const prompt = renderSystemPrompt(DEFAULT_SYSTEM_PROMPT_TEMPLATE, PERSONA);
    expect(prompt).not.toContain("долговременная память");
    expect(prompt).not.toContain("Отношение к собеседнику");
    expect(prompt).toContain("Сообщения пользователя идут пачкой");
  });

  it("неизвестная метка — ошибка (защита от опечатки в шаблоне)", () => {
    expect(() =>
      renderSystemPrompt("Привет, {{persona}}!\n\n{{moood}}", PERSONA),
    ).toThrow(/moood/);
  });

  it("HTML-комментарий в шаблоне не попадает в промпт", () => {
    const prompt = renderSystemPrompt(
      "<!-- инструкция редактору -->\n{{persona}}",
      PERSONA,
    );
    expect(prompt).not.toContain("инструкция редактору");
    expect(prompt).toContain("Ты — Dio");
  });

  it("персона-блок: имя, манера, ценности, биография помечена вымыслом", () => {
    const block = renderPersonaBlock(PERSONA);
    expect(block).toContain("Ты — Dio");
    expect(block).toContain("Манера речи:");
    expect(block).toContain("Ценности: честность");
    expect(block).toContain("вымысел");
  });

  it("пустая метка в середине абзаца убирает весь абзац, соседние остаются", () => {
    const prompt = renderSystemPrompt(
      "Первый абзац.\n\nГраница: {{relationship}}.\n\nТретий абзац.",
      PERSONA,
    );
    expect(prompt).toBe("Первый абзац.\n\nТретий абзац.");
  });

  it("метка с пробелами внутри распознаётся и заменяется ({{ mood }})", () => {
    const prompt = renderSystemPrompt("Состояние: {{ mood }}.", PERSONA, { moodLine: "бодр" });
    expect(prompt).toBe("Состояние: бодр.");
  });
});
