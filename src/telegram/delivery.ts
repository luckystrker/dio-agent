import type { Pool } from "pg";
import type { Logger } from "../runtime/logger.js";
import { withTimeout } from "../runtime/with-timeout.js";
import type { TelegramApi, TelegramApiError } from "./api.js";
import type { OutboxRepo, OutboxRow } from "./outbox.js";

// Delivery loop (§13): работает независимо от когнитивного worker,
// сохраняет порядок внутри чата. Политика неопределённости (§13.2):
// подтверждённый временный отказ — retry_wait (уважая retry_after);
// подтверждённая постоянная ошибка — failed; timeout/обрыв после
// возможной отправки — unknown без автоповтора, зависимые пузыри отменяются.

export interface DeliveryOptions {
  intervalMs?: number;
  batchSize?: number;
  sendTimeoutMs?: number;
  maxAttempts?: number;
  /** база экспоненциальной паузы retry, мс */
  retryBaseMs?: number;
}

const DEFAULTS = {
  intervalMs: 500,
  batchSize: 10,
  sendTimeoutMs: 10_000,
  maxAttempts: 5,
  retryBaseMs: 2_000,
};

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function isTelegramApiError(error: unknown): error is TelegramApiError {
  return error instanceof Error && error.name === "TelegramApiError";
}

export class DeliveryLoop {
  private readonly options: Required<DeliveryOptions>;
  private timer?: NodeJS.Timeout;
  private stopped = false;
  private busy = false;

  constructor(
    private readonly deps: {
      pool: Pool;
      repo: OutboxRepo;
      api: TelegramApi;
      logger: Logger;
    },
    options: DeliveryOptions = {},
  ) {
    this.options = { ...DEFAULTS, ...options };
  }

  start(): void {
    this.stopped = false;
    this.timer = setInterval(() => void this.tick(), this.options.intervalMs);
    this.deps.logger.debug("delivery loop запущен", { intervalMs: this.options.intervalMs });
  }

  async stop(): Promise<void> {
    this.stopped = true;
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
    while (this.busy) await sleep(50);
  }

  /** Один такт доставки — отдельным методом для тестируемости. */
  async tick(): Promise<void> {
    if (this.busy || this.stopped) return;
    this.busy = true;
    try {
      const rows = await this.deps.repo.claimDue(this.options.batchSize);
      for (const row of rows) {
        if (this.stopped) return;
        await this.deliver(row);
      }
    } catch (error) {
      this.deps.logger.error("delivery: ошибка такта", {
        error: error instanceof Error ? error.message : String(error),
      });
    } finally {
      this.busy = false;
    }
  }

  private async deliver(row: OutboxRow): Promise<void> {
    // §13.2/§13.3: захваченная пачка могла устареть в памяти — предыдущий
    // пузырь стал unknown и отменил этот. Перепроверяем статус перед отправкой.
    const currentStatus = await this.deps.repo.statusOf(row.id);
    if (currentStatus !== "sending") {
      this.deps.logger.debug("строка больше не в sending — пропуск", {
        turnId: row.turnId,
        position: row.position,
        currentStatus,
      });
      return;
    }

    // §13.3: перед отправкой — устаревание. Новые события в чате после
    // создания turn делают продолжение неуместным: оставшиеся пузыри отменяются.
    if (await this.chatHasNewEventsAfterTurn(row)) {
      const cancelled = await this.deps.repo.cancelTurnFrom(row.turnId, row.position, "чат ушёл вперёд — пузыри устарели");
      this.deps.logger.info("доставка отменена: чат ушёл вперёд", {
        turnId: row.turnId,
        position: row.position,
        cancelled,
      });
      return;
    }

    try {
      if (row.kind === "reaction") {
        if (row.targetMessageId === null || row.emoji === null) {
          await this.deps.repo.markFailed(row.id, "реакция без emoji/message_id");
          return;
        }
        await withTimeout(
          (signal) => this.deps.api.setMessageReaction(row.chatId, row.targetMessageId!, row.emoji!, signal),
          this.options.sendTimeoutMs,
        );
        await this.deps.repo.markSent(row.id, "reaction");
        this.deps.logger.info("реакция поставлена", {
          turnId: row.turnId,
          emoji: row.emoji,
          messageId: row.targetMessageId,
        });
        return;
      }
      const result = await withTimeout(
        (signal) => this.deps.api.sendMessage(row.chatId, row.text, signal),
        this.options.sendTimeoutMs,
      );
      await this.deps.repo.markSent(row.id, result.messageId);
      this.deps.logger.info("отправлено", { turnId: row.turnId, position: row.position, messageId: result.messageId });
    } catch (error) {
      await this.handleSendError(row, error);
    }
  }

  private async handleSendError(row: OutboxRow, error: unknown): Promise<void> {
    const message = error instanceof Error ? error.message : String(error);

    if (isTelegramApiError(error)) {
      if (error.httpStatus === 429) {
        const waitMs = (error.retryAfterSec ?? 1) * 1000;
        await this.deps.repo.markRetryWait(row.id, waitMs, message);
        return;
      }
      if (error.httpStatus >= 500) {
        await this.markTemporary(row, message);
        return;
      }
      // Подтверждённая постоянная ошибка (4xx): failed (§13.2)
      await this.deps.repo.markFailed(row.id, message);
      this.deps.logger.error("доставка провалилась навсегда", { turnId: row.turnId, position: row.position, message });
      return;
    }

    // Timeout / обрыв после возможной отправки: unknown, без автоповтора,
    // зависимые пузыри последовательности отменяются (§13.2)
    const cancelled = await this.deps.repo.markUnknown(row.id, message);
    this.deps.logger.warn("отправка неопределённа (unknown), зависимые отменены", {
      turnId: row.turnId,
      position: row.position,
      cancelledDependents: cancelled,
      message,
    });
  }

  private async markTemporary(row: OutboxRow, message: string): Promise<void> {
    if (row.attempts >= this.options.maxAttempts) {
      await this.deps.repo.markFailed(row.id, `превышен лимит попыток: ${message}`);
      this.deps.logger.error("доставка: лимит попыток исчерпан", { turnId: row.turnId, position: row.position });
      return;
    }
    const backoffMs = this.options.retryBaseMs * 2 ** (row.attempts - 1);
    await this.deps.repo.markRetryWait(row.id, backoffMs, message);
  }

  private async chatHasNewEventsAfterTurn(row: OutboxRow): Promise<boolean> {
    const result = await this.deps.pool.query<{ exists: boolean }>(
      `SELECT EXISTS (
         SELECT 1 FROM social_events
         WHERE chat_id = $1 AND turn_id IS NULL AND status = 'pending'
       ) AS exists`,
      [row.chatId],
    );
    return result.rows[0]?.["exists"] === true;
  }
}
