import { describe, expect, it, vi } from "vitest";
import { BackgroundScheduler } from "./scheduler.js";
import type { Budget } from "./budgets.js";
import type { Logger } from "./logger.js";

// Юнит-тесты планировщика без БД: бюджет через фейковый Budget,
// in-memory режим, границы дня, stop/tick, ошибки enqueue.

type Calls = { warn: Array<unknown[]>; error: Array<unknown[]>; debug: Array<unknown[]> };

function makeLogger(): { logger: Logger; calls: Calls } {
  const calls: Calls = { warn: [], error: [], debug: [] };
  const logger: Logger = {
    debug: (message, extra) => calls.debug.push([message, extra]),
    info: () => undefined,
    warn: (message, extra) => calls.warn.push([message, extra]),
    error: (message, extra) => calls.error.push([message, extra]),
  };
  return { logger, calls };
}

function makeBudget(options: { reserved?: number; reserveResult?: boolean } = {}): {
  budget: Budget;
  tryReserveCalls: string[];
  reservedTodayCalls: number;
} {
  const state = { reservedTodayCalls: 0 };
  const tryReserveCalls: string[] = [];
  const budget: Budget = {
    async tryReserve(operationId: string): Promise<boolean> {
      tryReserveCalls.push(operationId);
      return options.reserveResult ?? true;
    },
    async reservedToday(): Promise<number> {
      state.reservedTodayCalls++;
      return options.reserved ?? 0;
    },
  };
  return { budget, tryReserveCalls, ...state };
}

function warnText(calls: Calls, substring: string): boolean {
  return calls.warn.some(([message]) => String(message).includes(substring));
}

describe("BackgroundScheduler с бюджетом из БД (фейковый Budget)", () => {
  it("резерв получен → enqueue вызывается ровно один раз с нужным kind", async () => {
    const { logger } = makeLogger();
    const { budget, tryReserveCalls } = makeBudget({ reserved: 0, reserveResult: true });
    const enqueue = vi.fn().mockResolvedValue(undefined);
    const scheduler = new BackgroundScheduler({ enqueue, logger, budget }, { kind: "consolidation" });

    await scheduler.tick();

    expect(enqueue).toHaveBeenCalledTimes(1);
    expect(enqueue).toHaveBeenCalledWith("consolidation");
    expect(tryReserveCalls).toHaveLength(1);
    expect(tryReserveCalls[0]).toMatch(/^background:consolidation:/);
  });

  it("исчерпание: reservedToday >= max → enqueue НЕ вызывается, предупреждение записано", async () => {
    const { logger, calls } = makeLogger();
    const { budget, tryReserveCalls } = makeBudget({ reserved: 7, reserveResult: true });
    const enqueue = vi.fn().mockResolvedValue(undefined);
    const scheduler = new BackgroundScheduler(
      { enqueue, logger, budget },
      { maxJobsPerDay: 7 },
    );

    await scheduler.tick();

    expect(enqueue).not.toHaveBeenCalled();
    expect(tryReserveCalls).toHaveLength(0); // даже не пытаемся резервировать
    expect(warnText(calls, "исчерпан")).toBe(true);
  });

  it("tryReserve=false (операция уже резервировалась) → enqueue не вызывается, дубль не ставится", async () => {
    const { logger } = makeLogger();
    const { budget, tryReserveCalls } = makeBudget({ reserved: 0, reserveResult: false });
    const enqueue = vi.fn().mockResolvedValue(undefined);
    const scheduler = new BackgroundScheduler({ enqueue, logger, budget });

    await scheduler.tick();

    expect(enqueue).not.toHaveBeenCalled();
    expect(tryReserveCalls).toHaveLength(1); // сама попытка была
  });

  it("ошибка enqueue не бросается наружу из tick (логируется, процесс живёт)", async () => {
    const { logger, calls } = makeLogger();
    const { budget } = makeBudget({ reserved: 0, reserveResult: true });
    const enqueue = vi.fn().mockRejectedValue(new Error("БД недоступна"));
    const scheduler = new BackgroundScheduler({ enqueue, logger, budget });

    await expect(scheduler.tick()).resolves.toBeUndefined();
    expect(enqueue).toHaveBeenCalledTimes(1);
    expect(calls.error).toHaveLength(1);
  });

  it("tick после stop() — полный запрет: ни бюджета, ни enqueue", async () => {
    const { logger } = makeLogger();
    const { budget, tryReserveCalls } = makeBudget();
    const enqueue = vi.fn().mockResolvedValue(undefined);
    const scheduler = new BackgroundScheduler({ enqueue, logger, budget });
    scheduler.start();
    scheduler.stop();

    await scheduler.tick();

    expect(enqueue).not.toHaveBeenCalled();
    expect(tryReserveCalls).toHaveLength(0);
  });

  it("дефолтный интервал — 30 секунд (§14.2): два тика за минуту, не больше", async () => {
    vi.useFakeTimers();
    try {
      const { logger } = makeLogger();
      const { budget } = makeBudget();
      const enqueue = vi.fn().mockResolvedValue(undefined);
      const scheduler = new BackgroundScheduler({ enqueue, logger, budget });
      scheduler.start();
      try {
        await vi.advanceTimersByTimeAsync(29_999);
        expect(enqueue).not.toHaveBeenCalled();
        await vi.advanceTimersByTimeAsync(1);
        expect(enqueue).toHaveBeenCalledTimes(1);
        await vi.advanceTimersByTimeAsync(30_000);
        expect(enqueue).toHaveBeenCalledTimes(2);
      } finally {
        scheduler.stop();
      }
    } finally {
      vi.useRealTimers();
    }
  });

  it("stop() глушит интервал: после stop новых тиков нет", async () => {
    vi.useFakeTimers();
    try {
      const { logger } = makeLogger();
      const { budget } = makeBudget();
      const enqueue = vi.fn().mockResolvedValue(undefined);
      const scheduler = new BackgroundScheduler({ enqueue, logger, budget }, { intervalMs: 50 });
      scheduler.start();
      await vi.advanceTimersByTimeAsync(50);
      expect(enqueue).toHaveBeenCalledTimes(1);
      scheduler.stop();
      await vi.advanceTimersByTimeAsync(500);
      expect(enqueue).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("BackgroundScheduler без бюджета (in-memory режим)", () => {
  it("первый tick предупреждает про память ровно один раз", async () => {
    const { logger, calls } = makeLogger();
    const enqueue = vi.fn().mockResolvedValue(undefined);
    const scheduler = new BackgroundScheduler({ enqueue, logger }, { maxJobsPerDay: 100 });

    await scheduler.tick();
    await scheduler.tick();
    await scheduler.tick();

    expect(enqueue).toHaveBeenCalledTimes(3);
    expect(calls.warn.filter(([message]) => String(message).includes("памяти"))).toHaveLength(1);
  });

  it("исчерпание дневного лимита в памяти: сверх лимита enqueue не вызывается", async () => {
    const { logger, calls } = makeLogger();
    const enqueue = vi.fn().mockResolvedValue(undefined);
    const scheduler = new BackgroundScheduler({ enqueue, logger }, { maxJobsPerDay: 2 });

    await scheduler.tick();
    await scheduler.tick();
    await scheduler.tick();
    await scheduler.tick();

    expect(enqueue).toHaveBeenCalledTimes(2);
    expect(warnText(calls, "исчерпан")).toBe(true);
  });

  it("rollover дня: на новом календарном дне лимит обнуляется", async () => {
    vi.useFakeTimers({ now: new Date("2026-10-21T12:00:00") });
    try {
      const { logger } = makeLogger();
      const enqueue = vi.fn().mockResolvedValue(undefined);
      const scheduler = new BackgroundScheduler({ enqueue, logger }, { maxJobsPerDay: 1 });

      await scheduler.tick();
      expect(enqueue).toHaveBeenCalledTimes(1);

      await scheduler.tick(); // тот же день — лимит исчерпан
      expect(enqueue).toHaveBeenCalledTimes(1);

      vi.setSystemTime(new Date("2026-10-22T00:00:05")); // следующий день
      await scheduler.tick();
      expect(enqueue).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("ошибка enqueue в памяти не бросается наружу", async () => {
    const { logger, calls } = makeLogger();
    const enqueue = vi.fn().mockRejectedValue(new Error("очередь сломалась"));
    const scheduler = new BackgroundScheduler({ enqueue, logger });

    await expect(scheduler.tick()).resolves.toBeUndefined();
    expect(calls.error).toHaveLength(1);
  });
});

describe("BackgroundScheduler: базовая конфигурация", () => {
  it("kind по умолчанию — consolidation, кастомный kind пробрасывается в enqueue", async () => {
    const { logger } = makeLogger();
    const { budget } = makeBudget();
    const enqueue = vi.fn().mockResolvedValue(undefined);

    const defaults = new BackgroundScheduler({ enqueue, logger, budget });
    await defaults.tick();
    expect(enqueue).toHaveBeenLastCalledWith("consolidation");

    const custom = new BackgroundScheduler({ enqueue, logger, budget }, { kind: "exploration" });
    await custom.tick();
    expect(enqueue).toHaveBeenLastCalledWith("exploration");
  });
});
