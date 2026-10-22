import type { Persona, SystemPromptExtras } from "./persona.js";

/**
 * Каркас системного промпта (config/system-prompt.md) — редактируемый шаблон
 * (§7.1). Метки {{persona}}, {{mood}}, {{relationship}}, {{memory}} заменяются
 * на состояние из БД. Абзац, содержащий пустую метку, исчезает целиком:
 * недоступная часть состояния не оставляет сирот-инструкций. HTML-комментарии
 * вырезаются — в них можно оставлять подсказки редактору.
 */

export const DEFAULT_SYSTEM_PROMPT_TEMPLATE = `{{persona}}

Твоё состояние сейчас: {{mood}}.

Отношение к собеседнику: {{relationship}}.

Твоя долговременная память об этом чате (используй естественно, не выдумывай сверх записанного и не цитируй формат):
{{memory}}

Сообщения пользователя идут пачкой одного собеседника — отвечай на пачку целиком.`;

/** Текстовая персона-блок: кто я, манера речи, ценности, заданная биография. */
export function renderPersonaBlock(persona: Persona): string {
  const lines = [
    `Ты — ${persona.name}. ${persona.description}`,
    `Манера речи: ${persona.speechStyle}`,
    `Ценности: ${persona.values.join(", ")}.`,
  ];
  if (persona.seedBio) {
    lines.push(`Твоя заданная биография (вымысел для колорита, не пережитый опыт): ${persona.seedBio}`);
  }
  return lines.join("\n\n");
}

// Любимое содержимое между двойными фигурными скобками — метка; имя
// обрезается по краям. Широкий паттерн вместо \w+: метка с кириллицей или
// опечаткой тоже должна попасть в валидацию, а не просочиться в промпт
// сырым текстом (находка приёмки 7.1)
const PLACEHOLDER = /\{\{([^{}]*)\}\}/g;

function placeholderNames(text: string): string[] {
  return [...text.matchAll(PLACEHOLDER)].map((match) => (match[1] ?? "").trim());
}

/** Заменить метки шаблона; бросает при неизвестной метке (опечатка в шаблоне). */
export function renderSystemPrompt(
  template: string,
  persona: Persona,
  extras: SystemPromptExtras = {},
): string {
  // Значения — Map, а не объект: ключи вида constructor/__proto__ не должны
  // наследоваться из прототипа и «рендериться» в промпт (находка приёмки 7.1)
  const values = new Map<string, string>([
    ["persona", renderPersonaBlock(persona)],
    ["mood", extras.moodLine ?? ""],
    ["relationship", extras.relationshipLine ?? ""],
    ["memory", extras.memoryBlock ?? ""],
  ]);

  const clean = template.replace(/<!--[\s\S]*?-->/g, "").trim();
  const known = [...values.keys()].join(", ");
  for (const name of placeholderNames(clean)) {
    if (!values.has(name)) {
      throw new Error(`шаблон системного промпта: неизвестная метка {{${name}}} — допустимы: ${known}`);
    }
  }

  return clean
    .split(/\n\s*\n/)
    .filter((paragraph) => !placeholderNames(paragraph).some((name) => values.get(name) === ""))
    .map((paragraph) =>
      paragraph.replace(PLACEHOLDER, (_, raw: string) => values.get((raw ?? "").trim()) ?? ""),
    )
    .join("\n\n")
    .trim();
}
