import type { Logger } from "./logger.js";
import type { WorkKind } from "./priorities.js";

export interface SchedulerOptions {
  /** Как часто планировщик пытается поставить фоновую работу. */
  intervalMs?: number;
  /** Дневной бюджет фоновых заданий (§16: бюджеты обязательны). */
  maxJobsPerDay?: number;
  kind?: WorkKind;
}

const DEFAULTS = {
  intervalMs: 60 * 60 * 1000, // раз в час
  maxJobsPerDay: 24,
  kind: "consolidation" as const,
};

/**
 * Фоновый планировщик (ARCHITECTURE §5.2): создаёт/активирует работу,
 * сам модель не вызывает — исполнение идёт через единственный worker.
 * Дневной бюджет соблюдается явно, при исчерпании пишет в лог.
 */
export class BackgroundScheduler {
  private readonly intervalMs: number;
  private readonly maxJobsPerDay: number;
  private readonly kind: WorkKind;
  private timer?: NodeJS.Timeout;
  private stopped = false;
  private jobsToday = 0;
  private budgetDay = new Date().toDateString();

  constructor(
    private readonly enqueue: (kind: WorkKind) => Promise<void>,
    private readonly logger: Logger,
    options: SchedulerOptions = {},
  ) {
    this.intervalMs = options.intervalMs ?? DEFAULTS.intervalMs;
    this.maxJobsPerDay = options.maxJobsPerDay ?? DEFAULTS.maxJobsPerDay;
    this.kind = options.kind ?? DEFAULTS.kind;
  }

  start(): void {
    this.stopped = false;
    this.timer = setInterval(() => void this.tick(), this.intervalMs);
    this.logger.debug("планировщик запущен", {
      intervalMs: this.intervalMs,
      maxJobsPerDay: this.maxJobsPerDay,
      kind: this.kind,
    });
  }

  stop(): void {
    this.stopped = true;
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
  }

  private rolloverBudgetIfNewDay(): void {
    const today = new Date().toDateString();
    if (today !== this.budgetDay) {
      this.budgetDay = today;
      this.jobsToday = 0;
    }
  }

  private async tick(): Promise<void> {
    if (this.stopped) return;
    this.rolloverBudgetIfNewDay();
    if (this.jobsToday >= this.maxJobsPerDay) {
      this.logger.warn("дневной бюджет фоновых заданий исчерпан", {
        maxJobsPerDay: this.maxJobsPerDay,
      });
      return;
    }
    this.jobsToday++;
    try {
      await this.enqueue(this.kind);
      this.logger.debug("фоновая работа поставлена в очередь", { kind: this.kind, jobsToday: this.jobsToday });
    } catch (error) {
      this.jobsToday--; // постановка не удалась — бюджет не расходуем
      this.logger.error("не удалось поставить фоновую работу", {
        error: error instanceof Error ? error.message : String(error),
      });
    }
  }
}
