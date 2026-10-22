import type { TelegramApi } from "./api.js";

// 8.4 (§13.3): «печатает…» показывается, пока реально ожидается ответ хода:
// пульс шлёт chat action сразу и обновляет примерно раз в 4 секунды.
// Пульс не имеет права ронять ход: ошибки api глотаются.

export interface TypingPulse {
  start(chatId: string): void;
  stop(chatId: string): void;
}

export class TelegramTypingPulse implements TypingPulse {
  private readonly timers = new Map<string, NodeJS.Timeout>();

  constructor(
    private readonly api: Pick<TelegramApi, "sendChatAction">,
    private readonly intervalMs = 4_000,
  ) {}

  start(chatId: string): void {
    this.stop(chatId);
    const fire = (): void => {
      void this.api.sendChatAction(chatId, "typing").catch(() => undefined);
    };
    fire();
    this.timers.set(chatId, setInterval(fire, this.intervalMs));
  }

  stop(chatId: string): void {
    const timer = this.timers.get(chatId);
    if (timer) {
      clearInterval(timer);
      this.timers.delete(chatId);
    }
  }
}
