import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { parsePersonaFile } from "./persona-file.js";

// §7.1 (независимая приёмка по спецификации, не по коду):
// config/persona.md — текст личности. Секции «## Имя», «## Описание»,
// «## Манера речи», «## Ценности» (списком «- пункт»), «## Настроение (базовое)»
// (valence -1..1, arousal 0..1, socialEnergy 0..1), необязательная
// «## Биография (вымысел)». HTML-комментарии игнорируются. Заголовки
// переименовывать нельзя; неизвестная/пропущенная/пустая обязательная секция,
// число вне диапазона или не число — ошибка с именем секции/поля в сообщении.
// Пустая секция биографии = биографии нет (null).

// --- конструирование файла по шаблону спеки (входы для PBT строятся здесь) ---

interface FileFields {
  name: string;
  description: string;
  speechStyle: string;
  values: string[];
  valence: string;
  arousal: string;
  socialEnergy: string;
  /** undefined = секция отсутствует целиком */
  bio?: string;
}

function buildFile(f: FileFields): string {
  const sections: string[] = [
    `## Имя\n${f.name}`,
    `## Описание\n${f.description}`,
    `## Манера речи\n${f.speechStyle}`,
    `## Ценности\n${f.values.map((v) => `- ${v}`).join("\n")}`,
    `## Настроение (базовое)\nvalence: ${f.valence}\narousal: ${f.arousal}\nsocialEnergy: ${f.socialEnergy}`,
  ];
  if (f.bio !== undefined) sections.push(`## Биография (вымысел)\n${f.bio}`);
  return sections.join("\n\n") + "\n";
}

const VALID_FIELDS: FileFields = {
  name: "Дио",
  description: "Живой собеседник в Telegram.",
  speechStyle: "Кратко, живо, по-русски.",
  values: ["честность", "любопытство"],
  valence: "0.1",
  arousal: "0.4",
  socialEnergy: "0.6",
  bio: "Вырос в портовом городе.",
};

/** Сообщение об ошибке парсера ("" — если не бросил). */
function errorOf(fn: () => unknown): string {
  try {
    fn();
  } catch (e) {
    return e instanceof Error ? e.message : String(e);
  }
  return "";
}

// --- произвольные, но «честные» для спеки входы: однострочные тексты без
// структурной разметки (переносов, «## », «- » в начале, скобок меток) ---

const hasControlChar = (s: string): boolean => [...s].some((ch) => ch.charCodeAt(0) <= 0x1f || ch.charCodeAt(0) === 0x7f);

const cleanText = (s: string): boolean => s.length > 0 && s.trim() === s && !hasControlChar(s) && !/[{}<>]/.test(s);

const textArb = fc.string({ minLength: 1, maxLength: 24 }).filter(cleanText);
const valueItemArb = textArb.filter((s) => !s.startsWith("-"));

/** Десятичные числа строкой, ровно два знака после запятой (без экспоненты). */
const decimal = (min: number, max: number): fc.Arbitrary<string> =>
  fc.integer({ min, max }).map((n) => (n / 100).toFixed(2));

describe("parsePersonaFile — спецификация §7.1 (независимая приёмка)", () => {
  // --- юнит: формат и границы по спеке ---

  it("границы диапазонов включены: valence -1 и 1, arousal и socialEnergy 0 и 1 — валидны", () => {
    const parsed = parsePersonaFile(
      buildFile({ ...VALID_FIELDS, valence: "-1", arousal: "0", socialEnergy: "1", bio: undefined }),
    );
    expect(parsed.moodBaseline).toEqual({ valence: -1, arousal: 0, socialEnergy: 1 });
  });

  it("шаг за границу ловится: valence ±1.01, arousal/socialEnergy -0.01 и 1.01", () => {
    const cases: Array<[keyof Pick<FileFields, "valence" | "arousal" | "socialEnergy">, string]> = [
      ["valence", "1.01"],
      ["valence", "-1.01"],
      ["arousal", "1.01"],
      ["arousal", "-0.01"],
      ["socialEnergy", "1.01"],
      ["socialEnergy", "-0.01"],
    ];
    for (const [field, raw] of cases) {
      const msg = errorOf(() => parsePersonaFile(buildFile({ ...VALID_FIELDS, [field]: raw })));
      expect(msg, `${field} = ${raw} должен быть ошибкой`).not.toBe("");
    }
  });

  it("комментарий внутри тела секции игнорируется: текст секции остаётся чистым", () => {
    const file = buildFile({ ...VALID_FIELDS }).replace("## Имя\nДио", "## Имя\n<!-- заметка редактора -->\nДио");
    expect(parsePersonaFile(file).name).toBe("Дио");
  });

  it("биография из одних пробельных символов — биографии нет (null)", () => {
    const parsed = parsePersonaFile(buildFile({ ...VALID_FIELDS, bio: "   \n  " }));
    expect(parsed.seedBio).toBeNull();
  });

  it("биография из одного комментария — тоже null (комментарий игнорируется)", () => {
    const parsed = parsePersonaFile(buildFile({ ...VALID_FIELDS, bio: "<!-- биографии пока нет -->" }));
    expect(parsed.seedBio).toBeNull();
  });

  it("реальный config/persona.md разбирается, и комментарий-инструкция не протекает в поля", () => {
    const file = readFileSync(new URL("../../config/persona.md", import.meta.url), "utf8");
    const parsed = parsePersonaFile(file);
    expect(parsed.name).toBe("Dio");
    expect(parsed.values.length).toBeGreaterThan(0);
    expect(parsed.seedBio).not.toBeNull();
    for (const field of [parsed.name, parsed.description, parsed.speechStyle, parsed.seedBio ?? "", ...parsed.values]) {
      expect(field).not.toContain("<!--");
      expect(field).not.toContain("-->");
      expect(field.toLowerCase()).not.toContain("правила файла");
    }
  });

  // --- PBT по свойствам спеки ---

  it("PBT: любые непустые тексты полей сохраняются как есть (полный round-trip)", () => {
    fc.assert(
      fc.property(
        textArb,
        textArb,
        textArb,
        fc.array(valueItemArb, { minLength: 1, maxLength: 4 }),
        fc.array(textArb, { minLength: 1, maxLength: 3 }),
        decimal(-100, 100),
        decimal(0, 100),
        decimal(0, 100),
        (name, description, speechStyle, values, bioLines, valence, arousal, socialEnergy) => {
          const bio = bioLines.join("\n");
          const parsed = parsePersonaFile(
            buildFile({ name, description, speechStyle, values, valence, arousal, socialEnergy, bio }),
          );
          expect(parsed.name).toBe(name);
          expect(parsed.description).toBe(description);
          expect(parsed.speechStyle).toBe(speechStyle);
          expect(parsed.values).toEqual(values);
          expect(parsed.seedBio).toBe(bio);
          expect(parsed.moodBaseline).toEqual({
            valence: Number(valence),
            arousal: Number(arousal),
            socialEnergy: Number(socialEnergy),
          });
        },
      ),
    );
  });

  it("PBT: любое нарушение диапазона любого из трёх чисел ловится, сообщение называет поле/секцию", () => {
    // valence допустим в -1..1, arousal и socialEnergy — в 0..1; нарушители строятся под своё поле
    const badValence = fc.oneof(decimal(101, 10000), decimal(101, 10000).map((s) => `-${s}`));
    const badUnit = fc.oneof(decimal(101, 10000), decimal(1, 100).map((s) => `-${s}`));
    const badCases = fc.oneof(
      fc.tuple(fc.constant<"valence" | "arousal" | "socialEnergy">("valence"), badValence),
      fc.tuple(fc.constant<"valence" | "arousal" | "socialEnergy">("arousal"), badUnit),
      fc.tuple(fc.constant<"valence" | "arousal" | "socialEnergy">("socialEnergy"), badUnit),
    );
    fc.assert(
      fc.property(badCases, ([field, raw]) => {
        const msg = errorOf(() => parsePersonaFile(buildFile({ ...VALID_FIELDS, [field]: raw })));
        expect(msg, `${field} = ${raw} вне диапазона — должна быть ошибка`).not.toBe("");
        expect(msg).toMatch(/valence|arousal|socialEnergy|Настроение/);
      }),
    );
  });

  it("PBT: нечисловое значение настроения ловится с именем поля/секции", () => {
    const nonNumberArb = fc
      .string({ minLength: 1, maxLength: 12 })
      .filter((s) => cleanText(s) && Number.isNaN(Number(s)));
    fc.assert(
      fc.property(
        fc.constantFrom("valence", "arousal", "socialEnergy"),
        nonNumberArb,
        (field, raw) => {
          const msg = errorOf(() => parsePersonaFile(buildFile({ ...VALID_FIELDS, [field]: raw })));
          expect(msg, `${field} = «${raw}» не число — должна быть ошибка`).not.toBe("");
          expect(msg).toMatch(/valence|arousal|socialEnergy|Настроение/);
        },
      ),
    );
  });

  it("PBT: пустая обязательная секция (всё из пробелов или пустой список) ловится с именем секции", () => {
    const blankish = fc.oneof(
      fc.constant(""),
      fc.string({ minLength: 1, maxLength: 5 }).filter((s) => s.trim() === "" && !s.includes("\n")),
    );
    fc.assert(
      fc.property(
        fc.constantFrom("Имя", "Описание", "Манера речи", "Ценности", "Настроение (базовое)"),
        blankish,
        blankish,
        blankish,
        (section, w1, w2, w3) => {
          const fields: FileFields = { ...VALID_FIELDS };
          if (section === "Имя") fields.name = w1;
          else if (section === "Описание") fields.description = w1;
          else if (section === "Манера речи") fields.speechStyle = w1;
          else if (section === "Ценности") fields.values = w1.trim() === "" ? [] : [w1, w2];
          else {
            fields.valence = w1;
            fields.arousal = w2;
            fields.socialEnergy = w3;
          }
          const msg = errorOf(() => parsePersonaFile(buildFile(fields)));
          const expected: RegExp =
            section === "Настроение (базовое)"
              ? /Настроение|valence|arousal|socialEnergy/
              : section === "Ценности"
                ? /Ценности/
                : new RegExp(section.replace(/[()]/g, "\\$&"));
          expect(msg, `пустая секция «${section}» — должна быть ошибка с её именем`).toMatch(expected);
        },
      ),
    );
  });

  it("PBT: пропущенная обязательная секция ловится с её именем в сообщении", () => {
    fc.assert(
      fc.property(
        fc.constantFrom("Имя", "Описание", "Манера речи", "Ценности", "Настроение (базовое)"),
        (section) => {
          // секция вырезается из уже собранного валидного файла целиком
          const file = buildFileWithout({ ...VALID_FIELDS }, section);
          const msg = errorOf(() => parsePersonaFile(file));
          const expected: RegExp =
            section === "Настроение (базовое)" ? /Настроение|valence|arousal|socialEnergy/ : new RegExp(section);
          expect(msg, `пропущенная секция «${section}» — должна быть ошибка с её именем`).toMatch(expected);
        },
      ),
    );
  });

  // ключи Object.prototype: из-за них «неизвестная секция» может молча
  // проглатываться — этот отдельный баг закреплён юнит-тестом ниже
  const PROTOTYPE_KEYS = [
    "__proto__",
    "constructor",
    "hasOwnProperty",
    "isPrototypeOf",
    "propertyIsEnumerable",
    "toLocaleString",
    "toString",
    "valueOf",
  ];

  it("НАХОДКА §7.1: секция-ключ прототипа («## __proto__», «## constructor») должна быть ошибкой неизвестной секции — сейчас молча проглатывается", () => {
    for (const header of ["__proto__", "constructor"]) {
      const file = buildFile({ ...VALID_FIELDS }) + `\n## ${header}\nмусор\n`;
      const msg = errorOf(() => parsePersonaFile(file));
      expect(msg, `неизвестная секция «${header}» — должна быть ошибка с её именем`).toContain(header);
    }
  });

  it("PBT: неизвестная секция ловится, имя попадает в сообщение", () => {
    const known = ["Имя", "Описание", "Манера речи", "Ценности", "Настроение (базовое)", "Биография (вымысел)"];
    fc.assert(
      fc.property(
        textArb.filter((s) => !known.includes(s) && !PROTOTYPE_KEYS.includes(s)),
        (header) => {
          const file = buildFile({ ...VALID_FIELDS }) + `\n## ${header}\nчто-то лишнее\n`;
          expect(errorOf(() => parsePersonaFile(file))).toContain(header);
        },
      ),
    );
  });

  it("PBT: комментарии с произвольным содержимым не меняют результат разбора", () => {
    const commentBody = fc
      .string({ minLength: 0, maxLength: 40 })
      .filter((s) => !s.includes("<") && !s.includes("--") && !s.includes(">"));
    fc.assert(
      fc.property(commentBody, commentBody, (c1, c2) => {
        const base = buildFile({ ...VALID_FIELDS });
        const withComments = `<!-- ${c1} -->\n\n${base}\n<!-- ${c2} -->\n`;
        expect(parsePersonaFile(withComments)).toEqual(parsePersonaFile(base));
      }),
    );
  });
});

/** Собрать файл без секции (для проверки «пропущенная секция»). */
function buildFileWithout(f: FileFields, section: string): string {
  const clone: FileFields = { ...f };
  const full = buildFile(clone);
  const marker =
    section === "Имя"
      ? "## Имя"
      : section === "Описание"
        ? "## Описание"
        : section === "Манера речи"
          ? "## Манера речи"
          : section === "Ценности"
            ? "## Ценности"
            : "## Настроение (базовое)";
  const start = full.indexOf(marker);
  const next = full.indexOf("\n## ", start + 1);
  const cut = next === -1 ? full.slice(start).replace(/\n$/, "") : full.slice(start, next);
  return (full.slice(0, start) + full.slice(start + cut.length)).replace(/^\n+/, "").replace(/\n+$/, "\n");
}
