// Prefilter (§6.3): дешёвые правила до модели — process или journal_only.
// Прямой запрос не отбрасывается никогда; низкосигнальный шум группы
// остаётся в недавней истории, но не вызывает LLM.

import type { NormalizedEvent } from "./normalize.js";

export type IngestDecision = "pending" | "journal_only";

export function decideIngestStatus(event: NormalizedEvent): IngestDecision {
  if (!event.supported) return "journal_only";

  // Личка — всегда прямое общение
  if (event.chatType === "private") return "pending";

  // В группе: reply / mention / command — прямое обращение
  if (event.kind === "reply" || event.kind === "mention" || event.kind === "command") {
    return "pending";
  }

  // Пассивный шум группы и правки — в журнал без обязательного LLM
  return "journal_only";
}
