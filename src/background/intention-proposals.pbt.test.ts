import { describe, expect, it } from "vitest";
import fc from "fast-check";
import {
  INTENTION_KINDS,
  intentionDedupeKey,
  validateIntentionProposals,
  type IntentionKind,
} from "./intention-proposals.js";

// PBT намерений (§14.1). Границы delay и strict-поля покрыты юнитом
// intention-proposals.test.ts; здесь — свойства нормализации dedupe_key
// и устойчивость валидатора к произвольным валидным входам.

const kindArb: fc.Arbitrary<IntentionKind> = fc.constantFrom(...INTENTION_KINDS);

/** Telegram-чат: числовой id (домен targetChatId в проде). */
const chatArb = fc.integer({ min: 1, max: 1_000_000_000_000 }).map(String);

/** Произвольная тема; валидные темы для валидатора строятся отдельно. */
const anyTopicArb = fc.string({ minLength: 0, maxLength: 60 });

const whitespaceArb = fc
  .array(fc.constantFrom(" ", "\t", "\n", "\r", "\u00A0", "\u2028", "\uFEFF"), {
    minLength: 0,
    maxLength: 5,
  })
  .map((chars) => chars.join(""));

const validTopicArb = fc
  .string({ minLength: 1, maxLength: 60 })
  .filter((topic) => topic.trim().length > 0 && topic.length <= 200);
const validReasonArb = validTopicArb;
const delayArb = fc.double({ min: 0.06, max: 167, noNaN: true });

const proposalArb = fc.record({
  kind: kindArb,
  topic: validTopicArb,
  reason: validReasonArb,
  delay: delayArb,
});

describe("intentionDedupeKey: нормализация (§14.1 — перефразировки не размножают поиск)", () => {
  it("регистр темы не меняет ключ", () => {
    fc.assert(
      fc.property(kindArb, anyTopicArb, chatArb, (kind, topic, chat) => {
        const base = intentionDedupeKey(kind, topic, chat);
        expect(intentionDedupeKey(kind, topic.toLowerCase(), chat)).toBe(base);
        expect(intentionDedupeKey(kind, topic.toUpperCase(), chat)).toBe(base);
      }),
    );
  });

  it("внешние пробелы темы не меняют ключ", () => {
    fc.assert(
      fc.property(
        kindArb,
        anyTopicArb,
        chatArb,
        whitespaceArb,
        whitespaceArb,
        (kind, topic, chat, lead, tail) => {
          expect(intentionDedupeKey(kind, lead + topic + tail, chat)).toBe(
            intentionDedupeKey(kind, topic.trim(), chat),
          );
        },
      ),
    );
  });

  it("в реальном домене (числовые чаты) ключ различает вид, тему и чат", () => {
    fc.assert(
      fc.property(kindArb, kindArb, anyTopicArb, anyTopicArb, chatArb, chatArb, (k1, k2, t1, t2, c1, c2) => {
        const sameTopic = t1.trim().toLowerCase() === t2.trim().toLowerCase();
        if (k1 === k2 && sameTopic && c1 === c2) return; // нормализационный эквивалент
        expect(intentionDedupeKey(k1, t1, c1)).not.toBe(intentionDedupeKey(k2, t2, c2));
      }),
    );
  });
});

describe("validateIntentionProposals: свойства валидатора", () => {
  it("валидное предложение проходит и нормализуется (trim), delay без искажений", () => {
    fc.assert(
      fc.property(proposalArb, ({ kind, topic, reason, delay }) => {
        const [parsed] = validateIntentionProposals([
          { kind, topic: ` ${topic} `, reason: ` ${reason} `, delay_hours: delay },
        ]);
        expect(parsed).toEqual({ kind, topic: topic.trim(), reason: reason.trim(), delayHours: delay });
      }),
    );
  });

  it("список 1..2 валидных сохраняет длину и порядок", () => {
    fc.assert(
      fc.property(fc.array(proposalArb, { minLength: 1, maxLength: 2 }), (proposals) => {
        const parsed = validateIntentionProposals(
          proposals.map(({ kind, topic, reason, delay }) => ({
            kind,
            topic,
            reason,
            delay_hours: delay,
          })),
        );
        expect(parsed.map((p) => p.topic)).toEqual(proposals.map((p) => p.topic.trim()));
        expect(parsed.map((p) => p.kind)).toEqual(proposals.map((p) => p.kind));
      }),
    );
  });

  it("идемпотентность: повторная валидация wire-формы результата ничего не меняет", () => {
    fc.assert(
      fc.property(fc.array(proposalArb, { minLength: 0, maxLength: 2 }), (proposals) => {
        const once = validateIntentionProposals(
          proposals.map(({ kind, topic, reason, delay }) => ({ kind, topic, reason, delay_hours: delay })),
        );
        const twice = validateIntentionProposals(
          once.map((p) => ({ kind: p.kind, topic: p.topic, reason: p.reason, delay_hours: p.delayHours })),
        );
        expect(twice).toEqual(once);
      }),
    );
  });

  it("delay вне диапазона отклоняется при любом валидном остальном", () => {
    fc.assert(
      fc.property(
        proposalArb,
        fc.oneof(
          fc.double({ min: -1000, max: 0.049, noNaN: true }),
          fc.double({ min: 168.001, max: 100_000, noNaN: true }),
        ),
        ({ kind, topic, reason }, delay) => {
          expect(() =>
            validateIntentionProposals([{ kind, topic, reason, delay_hours: delay }]),
          ).toThrow();
        },
      ),
    );
  });

  it("тема из одних пробелов отклоняется", () => {
    fc.assert(
      fc.property(kindArb, validReasonArb, delayArb, whitespaceArb, (kind, reason, delay, ws) => {
        const blankTopic = ws.length > 0 ? ws : " ";
        expect(() =>
          validateIntentionProposals([{ kind, topic: blankTopic, reason, delay_hours: delay }]),
        ).toThrow(/topic/);
      }),
    );
  });

  it("постороннее поле отклоняется при любом валидном остальном", () => {
    fc.assert(
      fc.property(
        proposalArb,
        fc.constantFrom("id", "sourceIds", "entityIds", "priority", "notBefore", "x"),
        ({ kind, topic, reason, delay }, extra) => {
          const entry = { kind, topic, reason, delay_hours: delay, [extra]: 1 };
          expect(() => validateIntentionProposals([entry])).toThrow(/постороннее поле/);
        },
      ),
    );
  });
});
