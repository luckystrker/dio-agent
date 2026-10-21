// Предложения намерений из TurnResult (§10.1: до 2 намерений на ход, §14.1).
// Модель оперирует ОТНОСИТЕЛЬНЫМ временем (часы) — абсолютные даты и
// таймзоны ей не доверяем. Runtime пересчитывает в абсолютные notBefore.

export const INTENTION_KINDS = ["reply_later", "check_in", "share", "explore"] as const;
export type IntentionKind = (typeof INTENTION_KINDS)[number];

/** Начальный предел §10.1: до 2 намерений на ход. */
export const MAX_INTENTIONS_PER_TURN = 2;

/** notBefore = now + hours; границы: 3 минуты … 7 дней. */
export const MIN_DELAY_HOURS = 0.05;
export const MAX_DELAY_HOURS = 24 * 7;

/** Намерение живёт не дольше 72 часов (§14.1 expiresAt). */
export const INTENTION_TTL_HOURS = 72;

const MAX_TOPIC_LENGTH = 200;
const MAX_REASON_LENGTH = 500;

export interface IntentionProposal {
  kind: IntentionKind;
  topic: string;
  reason: string;
  delayHours: number;
}

export class IntentionProposalError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "IntentionProposalError";
  }
}

/**
 * Чистая валидация предложений модели (аналог validateMemoryCandidates).
 * Всё, что не прошло, — отклоняется целиком; мысль без валидного будущего
 * действия остаётся memory item (§14.1).
 */
export function validateIntentionProposals(input: unknown): IntentionProposal[] {
  if (!Array.isArray(input)) {
    throw new IntentionProposalError("intentions должен быть массивом");
  }
  if (input.length > MAX_INTENTIONS_PER_TURN) {
    throw new IntentionProposalError(`больше ${MAX_INTENTIONS_PER_TURN} намерений за ход`);
  }

  return input.map((entry, index) => {
    const prefix = `намерение #${index}`;
    if (typeof entry !== "object" || entry === null) {
      throw new IntentionProposalError(`${prefix}: ожидается объект`);
    }
    const record = entry as Record<string, unknown>;

    const allowed = new Set(["kind", "topic", "reason", "delay_hours"]);
    for (const key of Object.keys(record)) {
      if (!allowed.has(key)) {
        throw new IntentionProposalError(`${prefix}: постороннее поле "${key}"`);
      }
    }

    const kind = record["kind"];
    if (typeof kind !== "string" || !(INTENTION_KINDS as readonly string[]).includes(kind)) {
      throw new IntentionProposalError(`${prefix}: неизвестный вид "${String(kind)}"`);
    }

    const topic = record["topic"];
    if (typeof topic !== "string" || topic.trim().length === 0 || topic.length > MAX_TOPIC_LENGTH) {
      throw new IntentionProposalError(`${prefix}: topic 1..${MAX_TOPIC_LENGTH} символов`);
    }

    const reason = record["reason"];
    if (typeof reason !== "string" || reason.trim().length === 0 || reason.length > MAX_REASON_LENGTH) {
      throw new IntentionProposalError(`${prefix}: reason 1..${MAX_REASON_LENGTH} символов`);
    }

    const delayHours = record["delay_hours"];
    if (typeof delayHours !== "number" || !Number.isFinite(delayHours)) {
      throw new IntentionProposalError(`${prefix}: delay_hours должен быть числом часов`);
    }
    if (delayHours < MIN_DELAY_HOURS || delayHours > MAX_DELAY_HOURS) {
      throw new IntentionProposalError(
        `${prefix}: delay_hours вне диапазона ${MIN_DELAY_HOURS}..${MAX_DELAY_HOURS}`,
      );
    }

    return {
      kind: kind as IntentionKind,
      topic: topic.trim(),
      reason: reason.trim(),
      delayHours,
    };
  });
}

/** Ключ дедупликации §14.1: тип + тема + чат. */
export function intentionDedupeKey(kind: IntentionKind, topic: string, chatId: string): string {
  return `${kind}:${topic.trim().toLowerCase()}:${chatId}`;
}
