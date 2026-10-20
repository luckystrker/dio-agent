import type { Logger } from "./logger.js";
import type { Budget } from "./budgets.js";
import type { WorkKind } from "./priorities.js";

export interface SchedulerOptions {
  /** Как часто планировщик просыпается (§14.2: ориентировочно 15–30 с). */
  intervalMs?: number;
  /** Дневной бюджет фоновых заданий (§16: бюджеты обязательны). */
  maxJobsPerDay?: number;
  kind?: WorkKind;
}

const DEFAULTS = {
  intervalMs: 30_000,
  maxJobsPerDay: 24,
  kind: "consolidation" as const,
};

/**
 * Фоновый планировщик (ARCHITECTURE §5.2): создаёт/активирует работу,
 * сам модель не вызывает — исполнение идёт через единственный worker.
 *
 * Бюджет — в базе (budget_reservations, уникальность operation_id + day):
 * перезапуски процесса не размножают дневной лимит. Без внедрённого
 * бюджета работает в памяти и честно предупреждает об этом (для тестов).
 */
export class BackgroundScheduler {
  private readonly intervalMs: number;
  private readonly maxJobsPerDay: number;
  private readonly kind: WorkKind;
  private timer?: NodeJS.Timeout;
  private stopped = false;
  private warnedInMemoryBudget = false;
  private memoryJobsToday = 0;
  private memoryBudgetDay = new Date().toDateString();

  constructor(
    private readonly deps: {
      enqueue: (kind: WorkKind) => Promise<void>;
      logger: Logger;
      budget?: Budget;
    },
    options: SchedulerOptions = {},
  ) {
    this.intervalMs = options.intervalMs ?? DEFAULTS.intervalMs;
    this.maxJobsPerDay = options.maxJobsPerDay ?? DEFAULTS.maxJobsPerDay;
    this.kind = options.kind ?? DEFAULTS.kind;
  }

  start(): void {
    this.stopped = false;
    this.timer = setInterval(() => void this.tick(), this.intervalMs);
    this.deps.logger.debug("планировщик запущен", {
      intervalMs: this.intervalMs,
      maxJobsPerDay: this.maxJobsPerDay,
      kind: this.kind,
      budget: this.deps.budget ? "база" : "память",
    });
  }

  stop(): void {
    this.stopped = true;
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
  }

  /** Один такт планировщика — отдельным методом для тестируемости. */
  async tick(): Promise<void> {
    if (this.stopped) return;

    if (this.deps.budget) {
      const reserved = await this.deps.budget.reservedToday();
      if (reserved >= this.maxJobsPerDay) {
        this.deps.logger.warn("дневной бюджет фоновых заданий исчерпан", {
          reserved,
          maxJobsPerDay: this.maxJobsPerDay,
        });
        return;
      }
    } else {
      this.warnInMemoryBudgetOnce();
      this.rolloverMemoryBudgetIfNewDay();
      if (this.memoryJobsToday >= this.maxJobsPerDay) {
        this.deps.logger.warn("дневной бюджет фоновых заданий исчерпан (память)", {
          maxJobsPerDay: this.maxJobsPerDay,
        });
        return;
      }
    }

    const operationId = `background:${this.kind}:${new Date().toISOString()}`;
    if (this.deps.budget) {
      const reserved = await this.deps.budget.tryReserve(operationId);
      if (!reserved) return; // операция уже резервирована — дубль не ставим
    } else {
      this.memoryJobsToday++;
    }

    try {
      await this.deps.enqueue(this.kind);
      this.deps.logger.debug("фоновая работа поставлена в очередь", { kind: this.kind });
    } catch (error) {
      // Резерв остаётся потраченным — консервативная сторона ошибки.
      this.deps.logger.error("не удалось поставить фоновую работу", {
        error: error instanceof Error ? error.message : String(error),
      });
    }
  }

  private warnInMemoryBudgetOnce(): void {
    if (this.warnedInMemoryBudget) return;
    this.warnedInMemoryBudget = true;
    this.deps.logger.warn(
      "бюджет планировщика в памяти: перезапуски процесса сбрасывают дневной лимит (в проде внедряй BudgetRepo)",
    );
  }

  private rolloverMemoryBudgetIfNewDay(): void {
    const today = new Date().toDateString();
    if (today !== this.memoryBudgetDay) {
      this.memoryBudgetDay = today;
      this.memoryJobsToday = 0;
    }
  }
}
