import { describe, expect, it } from "vitest";
import { parsePersonaFile } from "./persona-file.js";

// Файл личности (config/persona.md) — источник истины: правка файла +
// перезапуск → новая версия персоны в БД. Парсер строгий: ошибка в файле
// валит запуск с понятным сообщением, а не тихо деградирует.

const VALID = `<!-- комментарий-инструкция для редактора — парсер его игнорирует -->
# Личность Dio

## Имя
Dio

## Описание
Живой собеседник в Telegram: любопытный, немного ироничный.

## Манера речи
Кратко, живо, по-русски; разговорные фразы.

## Ценности
- честность
- любопытство

## Настроение (базовое)
valence: 0.1
arousal: 0.4
socialEnergy: 0.6

## Биография (вымысел)
Вырос в портовом городе, любит запах дождя.
`;

describe("parsePersonaFile (§7.1: файл — источник персоны)", () => {
  it("полный файл разбирается во все поля", () => {
    const source = parsePersonaFile(VALID);
    expect(source.name).toBe("Dio");
    expect(source.description).toContain("любопытный");
    expect(source.speechStyle).toContain("по-русски");
    expect(source.values).toEqual(["честность", "любопытство"]);
    expect(source.moodBaseline).toEqual({ valence: 0.1, arousal: 0.4, socialEnergy: 0.6 });
    expect(source.seedBio).toBe("Вырос в портовом городе, любит запах дождя.");
  });

  it("без секции биографии seedBio = null — биография необязательна", () => {
    const source = parsePersonaFile(VALID.replace(/## Биография[\s\S]*$/, ""));
    expect(source.seedBio).toBeNull();
  });

  it("пустая биография — тоже null", () => {
    const source = parsePersonaFile(
      VALID.replace("Вырос в портовом городе, любит запах дождя.", "   "),
    );
    expect(source.seedBio).toBeNull();
  });

  it("многострочные тексты сохраняют переносы", () => {
    const source = parsePersonaFile(
      VALID.replace(
        "Живой собеседник в Telegram: любопытный, немного ироничный.",
        "Первая строка описания.\nВторая строка описания.",
      ),
    );
    expect(source.description).toBe("Первая строка описания.\nВторая строка описания.");
  });

  it("пропущена обязательная секция — ошибка называет секцию", () => {
    expect(() => parsePersonaFile(VALID.replace(/## Манера речи[\s\S]*?(?=## )/, ""))).toThrow(
      /Манера речи/,
    );
  });

  it("неизвестная секция — ошибка (защита от опечатки в заголовке)", () => {
    expect(() => parsePersonaFile(VALID + "\n## Биографияя\nтекст\n")).toThrow(/Биографияя/);
  });

  it("valence вне диапазона -1..1 — ошибка", () => {
    expect(() => parsePersonaFile(VALID.replace("valence: 0.1", "valence: 2"))).toThrow(/valence/);
  });

  it("arousal не число — ошибка с именем поля", () => {
    expect(() => parsePersonaFile(VALID.replace("arousal: 0.4", "arousal: много"))).toThrow(
      /arousal/,
    );
  });

  it("пустой список ценностей — ошибка", () => {
    expect(() => parsePersonaFile(VALID.replace(/## Ценности[\s\S]*?(?=## )/, "## Ценности\n\n"))).toThrow(
      /Ценности/,
    );
  });
});
