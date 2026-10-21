// Клиент Telegram Bot API с внедряемым транспортом (для тестов и dry-run).

import { readFile } from "node:fs/promises";

export interface TelegramUpdate {
  update_id: number;
  [key: string]: unknown;
}

export interface SendMessageResult {
  messageId: string;
}

export interface TelegramApi {
  getUpdates(offset: number, timeoutSec: number, signal: AbortSignal): Promise<TelegramUpdate[]>;
  sendMessage(chatId: string, text: string, signal: AbortSignal): Promise<SendMessageResult>;
}

export type HttpTransport = (
  url: string,
  init: { method: "POST"; headers: Record<string, string>; body: string; signal: AbortSignal },
) => Promise<Response>;

/** Подтверждённая ошибка API: статус известен, отправки не было. */
export class TelegramApiError extends Error {
  constructor(
    message: string,
    public readonly httpStatus: number,
    /** секунды из retry_after у 429 */
    public readonly retryAfterSec: number | null,
  ) {
    super(message);
    this.name = "TelegramApiError";
  }
}

export class HttpTelegramApi implements TelegramApi {
  private readonly baseUrl: string;

  constructor(
    botToken: string,
    private readonly apiBase: string = "https://api.telegram.org",
    private readonly transport: HttpTransport = (url, init) => fetch(url, init),
  ) {
    this.baseUrl = `${apiBase.replace(/\/+$/, "")}/bot${botToken}`;
  }

  async getUpdates(offset: number, timeoutSec: number, signal: AbortSignal): Promise<TelegramUpdate[]> {
    const json = await this.call("getUpdates", { offset, timeout: timeoutSec, allowed_updates: ["message", "edited_message"] }, signal);
    const result = (json as { result?: unknown })?.["result"];
    return Array.isArray(result) ? (result as TelegramUpdate[]) : [];
  }

  async sendMessage(chatId: string, text: string, signal: AbortSignal): Promise<SendMessageResult> {
    const json = await this.call("sendMessage", { chat_id: chatId, text }, signal);
    const result = (json as { result?: { message_id?: number } })?.["result"];
    const messageId = result?.["message_id"];
    if (typeof messageId !== "number") {
      throw new TelegramApiError("sendMessage вернул ответ без message_id", 200, null);
    }
    return { messageId: String(messageId) };
  }

  private async call(method: string, payload: unknown, signal: AbortSignal): Promise<unknown> {
    const response = await this.transport(`${this.baseUrl}/${method}`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(payload),
      signal,
    });
    const json: unknown = await response.json().catch(() => null);
    const ok = (json as { ok?: boolean })?.["ok"] === true;
    if (!response.ok || !ok) {
      const description = (json as { description?: string })?.["description"] ?? response.statusText;
      const parameters = (json as { parameters?: { retry_after?: number } })?.["parameters"];
      throw new TelegramApiError(
        `${method}: HTTP ${response.status} — ${description}`,
        response.status,
        parameters?.["retry_after"] ?? null,
      );
    }
    return json;
  }
}

/**
 * Сухой режим без сети и токена: getUpdates читает update-строки (JSONL)
 * из файла, sendMessage логирует и возвращает фиктивный id.
 * Файл пополняется скриптом feed — полный локальный конвейер через
 * настоящий poller/ingest.
 */
export class DryRunTelegramApi implements TelegramApi {
  public readonly sent: Array<{ chatId: string; text: string }> = [];

  constructor(
    private readonly updatesFile: string,
    private readonly log: (line: string) => void = (line) => console.log(line),
  ) {}

  async getUpdates(offset: number, timeoutSec: number): Promise<TelegramUpdate[]> {
    let content = "";
    try {
      content = await readFile(this.updatesFile, "utf8");
    } catch {
      // файла ещё нет — обновлений нет
    }
    const updates = content
      .split("\n")
      .map((line) => line.trim())
      .filter((line) => line.length > 0)
      .map((line) => JSON.parse(line) as TelegramUpdate)
      .filter((update) => typeof update.update_id === "number" && update.update_id >= offset)
      .sort((a, b) => a.update_id - b.update_id);
    if (updates.length === 0) {
      // имитация long poll: не крутиться вхолостую
      await new Promise((resolve) => setTimeout(resolve, Math.min(timeoutSec * 1000, 3_000)));
    }
    return updates;
  }

  async sendMessage(chatId: string, text: string): Promise<SendMessageResult> {
    this.sent.push({ chatId, text });
    this.log(`[dry-run → chat ${chatId}] ${text.replace(/\n/g, " ")}`);
    return { messageId: `dry-${this.sent.length}` };
  }
}
