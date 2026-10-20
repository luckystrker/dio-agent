import { z } from "zod";

// TurnResult этапа 1 с кросс-полями по §11.2:
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
    reason: { type: "string", maxLength: 500 },
  },
  required: ["decision"],
  additionalProperties: false,
} as const;
