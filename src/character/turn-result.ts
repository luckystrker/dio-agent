import { z } from "zod";

// Кандидаты памяти (§10.1): модель предлагает, runtime валидирует
// (validateMemoryCandidates) и назначает источники. Предел — 5 на ход.
const memoryCandidateObject = z
  .object({
    kind: z.enum(["claim", "experience", "thought", "preference"]),
    text: z.string().min(1).max(2000),
    entities: z
      .array(
        z
          .object({
            name: z.string().min(1).max(200),
            kind: z
              .enum(["person", "topic", "place", "object", "event", "project"])
              .default("topic"),
            role: z.enum(["subject", "participant", "place", "context"]).default("subject"),
          })
          .strict(),
      )
      .max(3)
      .default([]),
    salience: z.number().min(0).max(1).default(0.5),
    confidence: z.number().min(0).max(1).optional(),
  })
  .strict()
  .refine(
    (candidate) => candidate.kind === "claim" || candidate.confidence === undefined,
    { message: "confidence допустим только у claim", path: ["confidence"] },
  );

// Модели любят ставить confidence всем кандидатам подряд, а не только claim.
// Поле бессмысленно для experience/thought/preference — тихо снимаем его,
// вместо того чтобы валить весь ход из-за одного лишнего поля.
const memoryCandidateSchema = z.preprocess((raw) => {
  if (typeof raw === "object" && raw !== null && !Array.isArray(raw)) {
    const record = raw as Record<string, unknown>;
    if (record["kind"] !== "claim" && "confidence" in record) {
      const { confidence: _confidence, ...rest } = record;
      return rest;
    }
  }
  return raw;
}, memoryCandidateObject);

// Изменение настроения за ход (§7.2): три параметра, ограничены на ход;
// значимое изменение требует краткого основания. Runtime дополнительно
// сжимает дельту до ±MAX_MOOD_DELTA_PER_TURN.
const moodDeltaSchema = z
  .object({
    valence: z.number().min(-1).max(1).optional(),
    arousal: z.number().min(0).max(1).optional(),
    socialEnergy: z.number().min(0).max(1).optional(),
    reason: z.string().min(1).max(500),
  })
  .strict();

// Предложения намерений (§10.1 ≤2/ход, §14.1): относительное время в часах.
const intentionProposalSchema = z
  .object({
    kind: z.enum(["reply_later", "check_in", "share", "explore"]),
    topic: z.string().min(1).max(200),
    reason: z.string().min(1).max(500),
    delay_hours: z.number().min(0.05).max(168),
  })
  .strict();

// Изменение отношений (§7.3): affinity/trust меняются ограниченно и
// с основанием; familiarity растёт по runtime-правилу, моделью не задаётся.
const relationshipDeltaSchema = z
  .object({
    affinity: z.number().min(-1).max(1).optional(),
    trust: z.number().min(0).max(1).optional(),
    reason: z.string().min(1).max(500),
  })
  .strict();

// TurnResult (§11.2): answer — 1–4 сообщения; react — реакция на сообщение;
// ignore/defer — без отправляемых сообщений. Данные намерения для defer
// (notBefore) приходят с фоновым модулем (этап 5) — сознательное отложение.
// Лимиты строк (1.10): сообщение ≤ 4000 (граница Telegram), заметка ≤ 2000.
export const TurnResultSchema = z
  .object({
    decision: z.enum(["answer", "react", "ignore", "defer"]),
    messages: z.array(z.string().min(1).max(4000)).max(4).default([]),
    notes: z.array(z.string().min(1).max(2000)).max(5).default([]),
    memory: z.array(memoryCandidateSchema).max(5).default([]),
    reaction: z.string().min(1).max(64).optional(),
    intentions: z.array(intentionProposalSchema).max(2).default([]),
    /** defer: через сколько часов вернуться (§11.2 — reply_later с notBefore) */
    defer_hours: z.number().min(0.05).max(168).optional(),
    mood_delta: moodDeltaSchema.optional(),
    relationship_delta: relationshipDeltaSchema.optional(),
    reason: z.string().max(500).optional(),
  })
  .strict()
  .superRefine((result, ctx) => {
    if (result.decision === "answer" && result.messages.length === 0) {
      ctx.addIssue({
        code: "custom",
        path: ["messages"],
        message: "answer требует от 1 до 4 сообщений",
      });
    }
    if (result.decision === "react" && result.reaction === undefined) {
      ctx.addIssue({
        code: "custom",
        path: ["reaction"],
        message: "react требует reaction (эмодзи)",
      });
    }
    if (result.decision === "react" && result.messages.length > 0) {
      ctx.addIssue({
        code: "custom",
        path: ["messages"],
        message: "react не содержит сообщений",
      });
    }
    if (result.decision !== "answer" && result.decision !== "react" && result.messages.length > 0) {
      ctx.addIssue({
        code: "custom",
        path: ["messages"],
        message: "ignore/defer не содержат отправляемых сообщений",
      });
    }
    if (result.decision !== "react" && result.reaction !== undefined) {
      ctx.addIssue({
        code: "custom",
        path: ["reaction"],
        message: "reaction допустим только при decision=react",
      });
    }
    if (result.decision === "defer" && result.defer_hours === undefined) {
      ctx.addIssue({
        code: "custom",
        path: ["defer_hours"],
        message: "defer требует defer_hours (reply_later с notBefore, §11.2)",
      });
    }
    if (result.decision !== "defer" && result.defer_hours !== undefined) {
      ctx.addIssue({
        code: "custom",
        path: ["defer_hours"],
        message: "defer_hours допустим только при decision=defer",
      });
    }
  });

export type TurnResult = z.infer<typeof TurnResultSchema>;

// JSON Schema для forced tool-calling. Написана руками и обязана оставаться
// согласованной с TurnResultSchema (согласованность проверяется тестом).
export const TURN_RESULT_JSON_SCHEMA = {
  type: "object",
  properties: {
    decision: { type: "string", enum: ["answer", "react", "ignore", "defer"] },
    messages: { type: "array", items: { type: "string", minLength: 1, maxLength: 4000 }, maxItems: 4 },
    notes: { type: "array", items: { type: "string", minLength: 1, maxLength: 2000 }, maxItems: 5 },
    memory: {
      type: "array",
      maxItems: 5,
      description:
        "До 5 кандидатов долговременной памяти о ЭТОМ разговоре: факты о пользователе (claim), события (experience), твои мысли (thought), вкусы (preference). Источники не указывай — их назначает система. Пиши только значимое, не каждое сообщение.",
      items: {
        type: "object",
        properties: {
          kind: { type: "string", enum: ["claim", "experience", "thought", "preference"] },
          text: { type: "string", minLength: 1, maxLength: 2000 },
          entities: {
            type: "array",
            maxItems: 3,
            items: {
              type: "object",
              properties: {
                name: { type: "string", minLength: 1, maxLength: 200 },
                kind: {
                  type: "string",
                  enum: ["person", "topic", "place", "object", "event", "project"],
                },
                role: { type: "string", enum: ["subject", "participant", "place", "context"] },
              },
              required: ["name"],
              additionalProperties: false,
            },
          },
          salience: { type: "number", minimum: 0, maximum: 1 },
          confidence: { type: "number", minimum: 0, maximum: 1 },
        },
        required: ["kind", "text"],
        additionalProperties: false,
      },
    },
    reaction: { type: "string", minLength: 1, maxLength: 64, description: "Эмодзи-реакция на последнее сообщение (только при decision=react)" },
    intentions: {
      type: "array",
      maxItems: 2,
      description: "До 2 отложенных действий: вернуться к теме (reply_later), спросить как дела (check_in), поделиться находкой (share), изучить тему (explore). Время — относительное, в часах от сейчас.",
      items: {
        type: "object",
        properties: {
          kind: { type: "string", enum: ["reply_later", "check_in", "share", "explore"] },
          topic: { type: "string", minLength: 1, maxLength: 200 },
          reason: { type: "string", minLength: 1, maxLength: 500 },
          delay_hours: { type: "number", minimum: 0.05, maximum: 168 },
        },
        required: ["kind", "topic", "reason", "delay_hours"],
        additionalProperties: false,
      },
    },
    defer_hours: { type: "number", minimum: 0.05, maximum: 168, description: "При decision=defer: через сколько часов вернуться к ответу" },
    mood_delta: {
      type: "object",
      description: "Необязательное изменение твоего настроения этим ходом (до ±0.3 за параметр; при сильной эмоции не занижай — заметная дельта лучше, чем 0.05)",
      properties: {
        valence: { type: "number", minimum: -1, maximum: 1 },
        arousal: { type: "number", minimum: 0, maximum: 1 },
        socialEnergy: { type: "number", minimum: 0, maximum: 1 },
        reason: { type: "string", minLength: 1, maxLength: 500 },
      },
      required: ["reason"],
      additionalProperties: false,
    },
    relationship_delta: {
      type: "object",
      description: "Необязательное изменение отношения к собеседнику, с причиной; можно и в минус, если человек перегнул",
      properties: {
        affinity: { type: "number", minimum: -1, maximum: 1 },
        trust: { type: "number", minimum: 0, maximum: 1 },
        reason: { type: "string", minLength: 1, maxLength: 500 },
      },
      required: ["reason"],
      additionalProperties: false,
    },
    reason: { type: "string", maxLength: 500 },
  },
  required: ["decision"],
  additionalProperties: false,
} as const;
