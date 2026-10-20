import { z } from "zod";

// Минимальный TurnResult этапа 1. Расширяется на этапах 3–4
// (memory items, mood/relationship deltas, delivery actions) —
// адаптер валидирует по этой схеме ДО применения (§17: невалидный
// ответ не применяется, «лучшего усилия» нет).

export const TurnResultSchema = z.object({
  /** answer — ответить сообщениями; ignore — промолчать; defer — отложить. */
  decision: z.enum(["answer", "ignore", "defer"]),
  /** Сообщения к отправке (этап 2: уйдут в outbox). */
  messages: z.array(z.string().min(1)).max(5).default([]),
  /** Заметки о состоянии: станут дельтами append_note. */
  notes: z.array(z.string().min(1)).max(5).default([]),
  /** Краткое обоснование решения — для журнала и трассировки. */
  reason: z.string().max(500).optional(),
});

export type TurnResult = z.infer<typeof TurnResultSchema>;

// JSON Schema для forced tool-calling. Написана руками, чтобы не зависеть
// от возможностей конкретной версии zod; обязана оставаться согласованной
// с TurnResultSchema (проверяется тестом).
export const TURN_RESULT_JSON_SCHEMA = {
  type: "object",
  properties: {
    decision: { type: "string", enum: ["answer", "ignore", "defer"] },
    messages: { type: "array", items: { type: "string" }, maxItems: 5 },
    notes: { type: "array", items: { type: "string" }, maxItems: 5 },
    reason: { type: "string" },
  },
  required: ["decision"],
  additionalProperties: false,
} as const;
