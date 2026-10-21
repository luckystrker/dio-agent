import { z } from "zod";

// Кандидаты памяти (§10.1): модель предлагает, runtime валидирует
// (validateMemoryCandidates) и назначает источники. Предел — 5 на ход.
const memoryCandidateSchema = z
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

// TurnResult с кросс-полями по §11.2:
// answer — 1–4 сообщения; ignore/defer — без отправляемых сообщений.
// Данные намерения для defer (notBefore) приходят с фоновым модулем (этап 5);
// поле добавляется тогда — сейчас его отсутствие не валидируется сознательно.
// Адаптер валидирует по этой схеме ДО применения (§17: невалидный ответ
// не применяется, «лучшего усилия» нет).

export const TurnResultSchema = z
  .object({
    decision: z.enum(["answer", "ignore", "defer"]),
    messages: z.array(z.string().min(1)).max(4).default([]),
    notes: z.array(z.string().min(1)).max(5).default([]),
    memory: z.array(memoryCandidateSchema).max(5).default([]),
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
    if (result.decision !== "answer" && result.messages.length > 0) {
      ctx.addIssue({
        code: "custom",
        path: ["messages"],
        message: "ignore/defer не содержат отправляемых сообщений",
      });
    }
  });

export type TurnResult = z.infer<typeof TurnResultSchema>;

// JSON Schema для forced tool-calling. Написана руками и обязана оставаться
// согласованной с TurnResultSchema (minLength/maxItems/maxLength включены;
// согласованность проверяется тестом).
export const TURN_RESULT_JSON_SCHEMA = {
  type: "object",
  properties: {
    decision: { type: "string", enum: ["answer", "ignore", "defer"] },
    messages: { type: "array", items: { type: "string", minLength: 1 }, maxItems: 4 },
    notes: { type: "array", items: { type: "string", minLength: 1 }, maxItems: 5 },
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
    reason: { type: "string", maxLength: 500 },
  },
  required: ["decision"],
  additionalProperties: false,
} as const;
