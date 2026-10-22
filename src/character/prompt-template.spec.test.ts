import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import fc from "fast-check";
import type { Persona } from "./persona.js";
import { buildSystemPrompt } from "./persona.js";
import { DEFAULT_SYSTEM_PROMPT_TEMPLATE, renderPersonaBlock, renderSystemPrompt } from "./prompt-template.js";

// §7.1 (независимая приёмка по спецификации, не по коду):
// каркас системного промпта с метками {{persona}} (всегда заполнена),
// {{mood}}, {{relationship}}, {{memory}} (могут отсутствовать).
// Правила рендера: абзац (текст между пустыми строками), содержащий пустую
// метку, исчезает ЦЕЛИКОМ — вместе с инструкцией вокруг метки; непустые метки
// заменяются инлайн (значение может быть многострочным); неизвестная метка —
// ошибка рендера с именем метки; HTML-комментарии вырезаются и не попадают
// в промпт. Есть встроенный DEFAULT_SYSTEM_PROMPT_TEMPLATE с теми же метками.

function makePersona(overrides: Partial<Persona> = {}): Persona {
  return {
    id: "p1",
    version: 1,
    name: "Дио",
    description: "Живой собеседник в Telegram.",
    speechStyle: "Кратко, живо, по-русски.",
    values: ["честность", "любопытство"],
    moodBaseline: { valence: 0.1, arousal: 0.4, socialEnergy: 0.6 },
    seedBio: "Вырос в портовом городе.",
    ...overrides,
  };
}

const PERSONA = makePersona();

const LABELS = ["persona", "mood", "relationship", "memory"] as const;
const EMPTYABLE_LABELS = ["mood", "relationship", "memory"] as const;

describe("DEFAULT_SYSTEM_PROMPT_TEMPLATE — спецификация §7.1", () => {
  it("встроенный шаблон содержит все четыре метки", () => {
    for (const label of LABELS) {
      expect(DEFAULT_SYSTEM_PROMPT_TEMPLATE).toContain(`{{${label}}}`);
    }
  });

  it("без extras абзацы с пустыми метками исчезают из дефолтного шаблона вместе с инструкциями", () => {
    const result = renderSystemPrompt(DEFAULT_SYSTEM_PROMPT_TEMPLATE, PERSONA);
    expect(result).toContain(PERSONA.name);
    expect(result).not.toContain("{{");
    // инструкции вокруг пустых меток не должны остаться сиротами
    expect(result).not.toContain("Твоё состояние сейчас");
    expect(result).not.toContain("Отношение к собеседнику");
    expect(result).not.toContain("долговременную память");
  });

  it("с заполненными extras дефолтный шаблон собирается целиком", () => {
    const result = renderSystemPrompt(DEFAULT_SYSTEM_PROMPT_TEMPLATE, PERSONA, {
      moodLine: "настроение: спокойное",
      relationshipLine: "собеседник пока незнаком",
      memoryBlock: "мы говорили о дожде",
    });
    expect(result).toContain(PERSONA.name);
    expect(result).toContain("настроение: спокойное");
    expect(result).toContain("собеседник пока незнаком");
    expect(result).toContain("мы говорили о дожде");
    expect(result).not.toContain("{{");
  });
});

describe("renderSystemPrompt — спецификация §7.1 (независимая приёмка)", () => {
  it("абзац, где пустая метка соседствует с заполненной, исчезает целиком — вместе с заполненным значением", () => {
    const template = "До {{mood}} середина {{memory}} после\n\nХвост.";
    const result = renderSystemPrompt(template, PERSONA, { memoryBlock: "ЗНАЧЕНИЕ-ПАМЯТИ" });
    expect(result).not.toContain("До ");
    expect(result).not.toContain("середина");
    expect(result).not.toContain("ЗНАЧЕНИЕ-ПАМЯТИ");
    expect(result).toContain("Хвост.");
  });

  it("многострочное значение вставляется инлайн с сохранением переносов", () => {
    const template = "Состояние:\n{{mood}}\n\nДальше.";
    const result = renderSystemPrompt(template, PERSONA, { moodLine: "строка один\nстрока два" });
    expect(result).toContain("строка один\nстрока два");
    expect(result).toContain("Дальше.");
  });

  it("шаблон из одной пустой метки даёт пустой результат; из одной {{persona}} — только блок персоны", () => {
    expect(renderSystemPrompt("{{mood}}", PERSONA).trim()).toBe("");
    const only = renderSystemPrompt("{{persona}}", PERSONA);
    expect(only.trim()).toBe(renderPersonaBlock(PERSONA).trim());
  });

  it("неизвестная латинская метка — ошибка даже в абзаце с пустой известной меткой, имя метки в сообщении", () => {
    const render = () => renderSystemPrompt("{{mood}} и {{typoAddress}} конец", PERSONA);
    expect(render).toThrowError(/typoAddress/);
  });

  it("НАХОДКА §7.1: неизвестная метка кириллицей должна быть ошибкой рендера — сейчас молча остаётся в промпте сырым «{{текстом}}»", () => {
    // спека: «неизвестная метка — ошибка рендера с именем метки»; промпт не должен
    // покидать с непереваренными скобками — опечатка обязана валить запуск
    let message = "";
    let output: string | null = null;
    try {
      output = renderSystemPrompt("Абзац про {{МеткаОпечатка}} конец.", PERSONA, { moodLine: "есть" });
    } catch (e) {
      message = e instanceof Error ? e.message : String(e);
    }
    if (output !== null) expect(output, "метка не распознана и не выброшена, а протекла в промпт").not.toContain("{{");
    expect(message).toContain("МеткаОпечатка");
  });

  it("renderPersonaBlock: тексты персоны и ценности на месте; биография помечена как вымысел", () => {
    const block = renderPersonaBlock(PERSONA);
    expect(block).toContain(PERSONA.name);
    expect(block).toContain(PERSONA.description);
    expect(block).toContain(PERSONA.speechStyle);
    for (const value of PERSONA.values) expect(block).toContain(value);
    expect(block).toContain(PERSONA.seedBio!);
    // §7.1: художественная биография маркируется
    expect(block).toMatch(/вымыс|persona_seed/i);
  });

  it("renderPersonaBlock: без биографии — никакого маркера вымысла и никакого «null» в тексте", () => {
    const block = renderPersonaBlock(makePersona({ seedBio: null }));
    expect(block).not.toMatch(/вымыс|persona_seed/i);
    expect(block).not.toContain("null");
    expect(block).toContain(PERSONA.name);
  });

  it("buildSystemPrompt — обёртка: без шаблона берёт дефолт, с шаблоном передаёт как есть", () => {
    const extras = { moodLine: "бодрое" };
    expect(buildSystemPrompt(PERSONA, extras)).toBe(
      renderSystemPrompt(DEFAULT_SYSTEM_PROMPT_TEMPLATE, PERSONA, extras),
    );
    const custom = "Имя: {{persona}}";
    expect(buildSystemPrompt(PERSONA, {}, custom)).toBe(
      renderSystemPrompt(custom, PERSONA, {}),
    );
  });

  it("реальный config/system-prompt.md: комментарии-инструкции не попадают в промпт", () => {
    const template = readFileSync(new URL("../../config/system-prompt.md", import.meta.url), "utf8");
    const result = renderSystemPrompt(template, PERSONA, { moodLine: "спокойное" });
    expect(result).not.toContain("редактируется свободно");
    expect(result).not.toContain("опечатка");
    expect(result).not.toContain("<!--");
    expect(result).not.toContain("{{");
    expect(result).toContain(PERSONA.name);
  });

  // --- PBT по свойствам спеки ---

  // произвольные, но честные тексты: однострочные, без скобок меток и разметки
  const hasControlChar = (s: string): boolean =>
    [...s].some((ch) => ch.charCodeAt(0) <= 0x1f || ch.charCodeAt(0) === 0x7f);
  const cleanText = (s: string): boolean =>
    s.length > 0 && s.trim() === s && !hasControlChar(s) && !/[{}<>]/.test(s);
  const textArb = fc.string({ minLength: 1, maxLength: 20 }).filter(cleanText);
  const linesArb = fc.array(textArb, { minLength: 1, maxLength: 3 }).map((lines) => lines.join("\n"));

  /** Абзац: от 1 до 4 частей — статичный текст или известная метка. */
  const paragraphParts = fc.array(
    fc.oneof(textArb, fc.constantFrom(...LABELS).map((l) => `{{${l}}}`)),
    { minLength: 1, maxLength: 4 },
  );
  const templateArb = fc.array(paragraphParts, { minLength: 1, maxLength: 5 });

  const filledPersonaArb = fc.record({
    name: textArb,
    description: textArb,
    speechStyle: textArb,
    values: fc.array(textArb, { minLength: 1, maxLength: 3 }),
  });

  it("PBT: результат никогда не содержит «{{» — что бы ни было в шаблоне из известных меток", () => {
    fc.assert(
      fc.property(
        templateArb,
        filledPersonaArb,
        fc.array(linesArb, { minLength: 3, maxLength: 3 }),
        fc.boolean(),
        (paragraphs, personaFields, [moodValue, relValue, memValue], filled) => {
          const persona = makePersona({
            ...personaFields,
            seedBio: null,
          });
          const extras = filled
            ? { moodLine: moodValue, relationshipLine: relValue, memoryBlock: memValue }
            : {};
          const result = renderSystemPrompt(paragraphs.map((p) => p.join(" ")).join("\n\n"), persona, extras);
          expect(result).not.toContain("{{");
          if (filled) {
            // непустые метки заменяются инлайн: значения присутствуют в результате —
            // но только если соответствующая метка вообще была в шаблоне
            const labels = new Set(
              paragraphs.flat().filter((p) => p.startsWith("{{")).map((p) => p.slice(2, -2)),
            );
            if (labels.has("mood")) expect(result).toContain(moodValue);
            if (labels.has("relationship")) expect(result).toContain(relValue);
            if (labels.has("memory")) expect(result).toContain(memValue);
          }
          // статичные абзацы (без меток) остаются без изменений
          for (const parts of paragraphs) {
            const text = parts.join(" ");
            if (!parts.some((p) => p.startsWith("{{"))) expect(result).toContain(text);
          }
        },
      ),
    );
  });

  it("PBT: абзац с пустой меткой исчезает целиком вместе со своим статичным текстом", () => {
    // точный фильтр: маркер не должен встречаться ни в одном тексте, который
    // гарантированно останется в выводе (блок персоны, шапка, подвал)
    const reservedText = renderPersonaBlock(PERSONA) + " Шапка. Подвал.";
    const markerArb = textArb.filter((m) => !reservedText.includes(m));
    fc.assert(
      fc.property(markerArb, fc.constantFrom(...EMPTYABLE_LABELS), fc.array(markerArb, { minLength: 0, maxLength: 2 }), (marker, label, after) => {
        const target = [marker, `{{${label}}}`, ...after].join(" ");
        const template = `Шапка. {{persona}}\n\n${target}\n\nПодвал.`;
        const result = renderSystemPrompt(template, PERSONA, {});
        expect(result).toContain("Шапка.");
        expect(result).toContain("Подвал.");
        expect(result, `абзац «${target}» должен исчезнуть целиком`).not.toContain(marker);
        for (const tail of after) expect(result).not.toContain(tail);
      }),
    );
  });

  it("PBT: неизвестная метка всегда бросает ошибку с её именем", () => {
    // имена «распознаваемой формы» (латиница/цифры/подчёркивание): ровно тот случай,
    // который рендерер обязан проверять. Ключи прототипа Object вынесены в отдельный
    // юнит-тест ниже: там у рендерера собственная дыра.
    const known = new Set<string>(LABELS);
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
    const unknownArb = fc
      .string({ minLength: 1, maxLength: 16 })
      .filter((s) => /^[a-zA-Z_][a-zA-Z0-9_]*$/.test(s) && !known.has(s) && !PROTOTYPE_KEYS.includes(s));
    fc.assert(
      fc.property(unknownArb, (unknown) => {
        const template = `Вступление {{persona}}.\n\nЧто-то про {{${unknown}}} в тексте.\n\nКонец.`;
        let message = "";
        try {
          renderSystemPrompt(template, PERSONA, { moodLine: "заполнено" });
        } catch (e) {
          message = e instanceof Error ? e.message : String(e);
        }
        expect(message, `метка ${unknown} неизвестна — должна быть ошибка`).not.toBe("");
        expect(message).toContain(unknown);
      }),
    );
  });

  it("НАХОДКА §7.1: метка-ключ прототипа {{constructor}} должна быть ошибкой — сейчас подставляет function Object() прямо в промпт", () => {
    let message = "";
    let output: string | null = null;
    try {
      output = renderSystemPrompt("Абзац про {{constructor}} конец.", PERSONA, { moodLine: "есть" });
    } catch (e) {
      message = e instanceof Error ? e.message : String(e);
    }
    if (output !== null) {
      expect(output, "метка не выброшена — унаследованная функция утекла в промпт").not.toMatch(
        /function Object|\[native code\]/,
      );
    }
    expect(message).toContain("constructor");
  });

  it("PBT: HTML-комментарии с произвольным содержимым не попадают в вывод", () => {
    // содержимое проверяем только отличимое от фиксированных текстов вокруг
    const reserved = ["Дио", "Telegram", "по-русски", "честность", "любопытство", "Порт", "Первое", "Второе", "бодрое"];
    const commentBody = fc
      .string({ minLength: 3, maxLength: 40 })
      .filter(
        (s) =>
          s.trim() === s &&
          !s.includes("<") &&
          !s.includes(">") &&
          !s.includes("--") &&
          !reserved.some((t) => t.includes(s) || s.includes(t)),
      );
    fc.assert(
      fc.property(commentBody, commentBody, (c1, c2) => {
        const template = `<!-- ${c1} -->\n\nПервое. {{persona}}\n\n<!-- ${c2} -->\n\nВторое. {{mood}}\n`;
        const result = renderSystemPrompt(template, PERSONA, { moodLine: "бодрое" });
        expect(result).not.toContain(`<!-- ${c1} -->`);
        expect(result).not.toContain(`<!-- ${c2} -->`);
        expect(result).not.toContain("<!--");
        expect(result).not.toContain(c1);
        expect(result).not.toContain(c2);
        expect(result).toContain("Первое.");
        expect(result).toContain("бодрое");
      }),
    );
  });
});
