// Нормализация Telegram update → событие (§6.1, шаг 1).
// Чистые функции: безопасное извлечение метаданных из raw JSON.

export type ChatType = "private" | "group";
export type EventKind = "message" | "reply" | "mention" | "command" | "edited" | "other";

export interface NormalizedEvent {
  telegramUpdateId: string;
  chatId: string;
  chatType: ChatType;
  actorId: string | null;
  kind: EventKind;
  text: string | null;
  /** false — update не поддерживается: журнал без обработки, offset не застревает */
  supported: boolean;
}

interface TelegramEntity {
  type?: string;
  offset?: number;
  length?: number;
}

interface TelegramMessage {
  from?: { id?: number };
  text?: string;
  entities?: TelegramEntity[];
  reply_to_message?: unknown;
  chat?: { id?: number; type?: string };
}

interface TelegramUpdate {
  update_id?: number;
  message?: TelegramMessage;
  edited_message?: TelegramMessage;
  channel_post?: TelegramMessage;
  edited_channel_post?: TelegramMessage;
  callback_query?: unknown;
  [key: string]: unknown;
}

function extractMessage(update: TelegramUpdate): { message: TelegramMessage | undefined; edited: boolean } {
  if (update.message) return { message: update.message, edited: false };
  if (update.edited_message) return { message: update.edited_message, edited: true };
  if (update.channel_post) return { message: update.channel_post, edited: false };
  if (update.edited_channel_post) return { message: update.edited_channel_post, edited: true };
  return { message: undefined, edited: false };
}

function detectKind(message: TelegramMessage, edited: boolean): EventKind {
  if (edited) return "edited";
  if (message.reply_to_message !== undefined) return "reply";
  const entityTypes = new Set((message.entities ?? []).map((entity) => entity.type ?? ""));
  if (entityTypes.has("bot_command")) return "command";
  if (entityTypes.has("mention")) return "mention";
  return "message";
}

export class NormalizationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "NormalizationError";
  }
}

/**
 * Нормализует update. Бросает NormalizationError только если update целиком
 * неразборчив (нет update_id) — такое не записать даже в журнал.
 * Неподдерживаемые типы (callback_query и т.п.) возвращаются с supported=false:
 * они фиксируются как journal_only, чтобы offset не застревал (§6.1).
 */
export function normalizeUpdate(raw: unknown): NormalizedEvent {
  if (typeof raw !== "object" || raw === null) {
    throw new NormalizationError("update не является объектом");
  }
  const update = raw as TelegramUpdate;
  const updateId = update.update_id;
  if (typeof updateId !== "number" || !Number.isSafeInteger(updateId)) {
    throw new NormalizationError("у update нет целочисленного update_id");
  }

  const { message, edited } = extractMessage(update);
  const chat = message?.chat;

  if (!message || typeof chat?.id !== "number") {
    // Неподдерживаемое: в журнал под фиктивным чатом 0, без обработки
    return {
      telegramUpdateId: String(updateId),
      chatId: "0",
      chatType: "group",
      actorId: null,
      kind: "other",
      text: null,
      supported: false,
    };
  }

  const chatType: ChatType = chat.type === "private" ? "private" : "group";
  const actorId = typeof message.from?.id === "number" ? String(message.from.id) : null;
  const kind = detectKind(message, edited);

  return {
    telegramUpdateId: String(updateId),
    chatId: String(chat.id),
    chatType,
    actorId,
    kind,
    text: typeof message.text === "string" ? message.text : null,
    supported: true,
  };
}
