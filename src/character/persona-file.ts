import type { Persona } from "./persona.js";

/**
 * Файл личности (config/persona.md) — источник истины (§7.1): правка файла
 * и перезапуск процесса создают новую версию персоны в БД. Парсер строгий:
 * любая ошибка в файле валит запуск с сообщением, называющим секцию/поле.
 */

export type PersonaSource = Omit<Persona, "id" | "version">;

const REQUIRED_SECTIONS = ["Имя", "Описание", "Манера речи", "Ценности", "Настроение (базовое)"] as const;
const KNOWN_SECTIONS: readonly string[] = [...REQUIRED_SECTIONS, "Биография (вымысел)"];

// Секции — Map, а не объект: ключи вида __proto__/constructor не должны
// наследоваться из прототипа и выглядеть «известными» (находка приёмки 7.1)
function splitSections(text: string): Map<string, string> {
  const withoutComments = text.replace(/<!--[\s\S]*?-->/g, "");
  const sections = new Map<string, string>();
  const parts = withoutComments.split(/^## /m).slice(1);
  for (const part of parts) {
    const newline = part.indexOf("\n");
    const header = part.slice(0, newline === -1 ? undefined : newline).trim();
    const body = newline === -1 ? "" : part.slice(newline + 1).trim();
    if (!sections.has(header)) sections.set(header, body);
  }
  return sections;
}

function parseMoodLine(body: string, field: string, min: number, max: number): number {
  const match = body.match(new RegExp(`^${field}\\s*:\\s*(.+)$`, "m"));
  const raw = match?.[1];
  if (!match || raw === undefined) throw new Error(`секция «Настроение (базовое)»: нет строки ${field}:`);
  const value = Number(raw.trim().replace(",", "."));
  if (!Number.isFinite(value) || value < min || value > max) {
    throw new Error(
      `секция «Настроение (базовое)»: ${field} должно быть числом от ${min} до ${max}, получено: ${raw.trim()}`,
    );
  }
  return value;
}

/** Разобрать текст config/persona.md в источник персоны. Бросает при любой ошибке. */
export function parsePersonaFile(text: string): PersonaSource {
  const sections = splitSections(text);

  const unknown = [...sections.keys()].filter((header) => !KNOWN_SECTIONS.includes(header));
  if (unknown.length > 0) {
    throw new Error(
      `persona-файл: неизвестная секция «${unknown[0]}» — допустимы: ${KNOWN_SECTIONS.join(", ")}`,
    );
  }
  const missing = REQUIRED_SECTIONS.filter((header) => !sections.has(header));
  if (missing.length > 0) {
    throw new Error(`persona-файл: нет обязательной секции «${missing[0]}»`);
  }
  const section = (header: string): string => sections.get(header) ?? "";

  const name = section("Имя");
  const description = section("Описание");
  const speechStyle = section("Манера речи");
  if (!name || !description || !speechStyle) {
    throw new Error("persona-файл: секции «Имя», «Описание» и «Манера речи» не должны быть пустыми");
  }

  const values = section("Ценности")
    .split("\n")
    .map((line) => line.replace(/^-\s*/, "").trim())
    .filter((line) => line.length > 0);
  if (values.length === 0) {
    throw new Error("persona-файл: секция «Ценности» пуста — нужен хотя бы один пункт списком (- пункт)");
  }

  const mood = section("Настроение (базовое)");
  const moodBaseline = {
    valence: parseMoodLine(mood, "valence", -1, 1),
    arousal: parseMoodLine(mood, "arousal", 0, 1),
    socialEnergy: parseMoodLine(mood, "socialEnergy", 0, 1),
  };

  // биография необязательна; пустая секция = нет биографии
  const seedBioRaw = sections.get("Биография (вымысел)") ?? "";
  const seedBio = seedBioRaw.trim().length > 0 ? seedBioRaw : null;

  return { name, description, speechStyle, values, moodBaseline, seedBio };
}
