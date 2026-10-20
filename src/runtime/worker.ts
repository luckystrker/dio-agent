import type { Logger } from "./logger.js";
import { decideRecoveryAction } from "./work-queue.js";
import type { WorkItem, WorkQueueRepo } from "./work-queue.js";
import { WORK_MAX_ATTEMPTS } from "./priorities.js";
import { withTimeout } from "./with-timeout.js";

export type WorkHandler = (item: WorkItem, signal: AbortSignal) => Promise<void>;

export interface WorkerOptions {
  /** Пауза между опросами очереди, когда работы нет. */
  pollIntervalMs?: number;
  /** Верхняя граница выполнения одного обработчика (§5.2, §16). */
  handlerTimeoutMs?: number;
}

const DEFAULTS = {
  pollIntervalMs: 500,
  handlerTimeoutMs: 120_000,
};

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Единственный когнитивный worker: в каждый момент выполняется не более
 * одного задания (ARCHITECTURE §5.2). Следующий шаг видит результаты
 * предыдущего — гонок нет по построению.
 */
export class CognitiveWorker {
  private readonly pollIntervalMs: number;
  private readonly handlerTimeoutMs: number;
  private stopped = false;
  private current?: Promise<void>;

  constructor(
    private readonly repo: WorkQueueRepo,
    private readonly handlers: Record<string, WorkHandler>,
    private readonly logger: Logger,
    options: WorkerOptions = {},
  ) {
    this.pollIntervalMs = options.pollIntervalMs ?? DEFAULTS.pollIntervalMs;
    this.handlerTimeoutMs = options.handlerTimeoutMs ?? DEFAULTS.handlerTimeoutMs;
  }

  start(): void {
    this.stopped = false;
    void this.loop();
  }

  /**
   * Остановить приём новой работы и ограниченно дождаться текущей
   * (граница — handlerTimeoutMs, §5.3).
   */
  async stop(): Promise<void> {
    this.stopped = true;
    if (this.current) await this.current;
  }

  private async loop(): Promise<void> {
    while (!this.stopped) {
      try {
        const item = await this.repo.claimNext();
        if (!item) {
          await sleep(this.pollIntervalMs);
          continue;
        }
        this.current = this.process(item);
        await this.current;
        this.current = undefined;
      } catch (error) {
        // Ошибка самой очереди (БД): логируем и ждём с удвоенной паузой.
        this.current = undefined;
        this.logger.error("worker: ошибка цикла очереди", {
          error: error instanceof Error ? error.message : String(error),
        });
        await sleep(this.pollIntervalMs * 2);
      }
    }
  }

  private async process(item: WorkItem): Promise<void> {
    this.logger.debug("worker взял работу", { id: item.id, kind: item.kind, attempt: item.attempts });
    const handler = this.handlers[item.kind];
    try {
      if (!handler) {
        throw new Error(`нет обработчика для kind="${item.kind}"`);
      }
      await withTimeout((signal) => handler(item, signal), this.handlerTimeoutMs);
      await this.repo.complete(item.id);
      this.logger.debug("работа завершена", { id: item.id, kind: item.kind });
    } catch (error) {
      const reason = error instanceof Error ? error.message : String(error);
      // Попытка уже посчитана при claim: решаем requeue или терминальный пропуск.
      if (decideRecoveryAction(item.attempts, WORK_MAX_ATTEMPTS) === "skip") {
        await this.repo.skip(item.id, `превышен лимит попыток: ${reason}`);
        this.logger.warn("работа помечена терминальным пропуском", { id: item.id, kind: item.kind, reason });
      } else {
        await this.repo.requeue(item.id);
        this.logger.warn("работа возвращена в очередь", { id: item.id, kind: item.kind, attempt: item.attempts, reason });
      }
    }
  }
}
