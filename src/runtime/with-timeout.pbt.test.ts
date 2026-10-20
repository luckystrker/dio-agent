import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { TimeoutError, withTimeout } from "./with-timeout.js";

// PBT по §5.2/§16/§17: каждый внешний вызов ограничен ТАЙМАУТОМ-СТЕНОЙ.
// Обёртка обязана завершаться в границу TimeoutError, даже если fn
// игнорирует AbortSignal или вовсе никогда не завершается; осиротевший
// (поздний) результат fn не применяется и не роняет процесс.

const timeoutArb = fc.integer({ min: 30, max: 200 });
const slowTimeoutArb = fc.integer({ min: 100, max: 400 });

/** Запас на шум таймеров/CI: обёртка не должна добавлять заметной задержки. */
const MARGIN_MS = 150;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

describe("PBT: withTimeout — прохождение результата и ошибок", () => {
  it("любое значение проходит насквозь без изменения", async () => {
    await fc.assert(
      fc.asyncProperty(fc.jsonValue({ maxDepth: 4 }), timeoutArb, async (value, timeoutMs) => {
        const result = await withTimeout(() => Promise.resolve(value), timeoutMs);
        expect(result).toEqual(value);
      }),
      { numRuns: 25 },
    );
  });

  it("ошибка fn пробрасывается как есть (не глотается и не перекрашивается)", async () => {
    await fc.assert(
      fc.asyncProperty(fc.string({ maxLength: 50, unit: "grapheme" }), timeoutArb, async (message, timeoutMs) => {
        const error = new Error(message);
        await expect(
          withTimeout(() => Promise.reject(error), timeoutMs),
        ).rejects.toBe(error);
      }),
      { numRuns: 25 },
    );
  });

  it("успешный fn не задерживается обёрткой: общее время ≈ времени fn", async () => {
    await fc.assert(
      fc.asyncProperty(slowTimeoutArb, timeoutArb, async (fnMs, timeoutMs) => {
        fc.pre(fnMs < timeoutMs);
        const startedAt = Date.now();
        await withTimeout(() => sleep(fnMs).then(() => undefined), timeoutMs);
        expect(Date.now() - startedAt).toBeLessThan(fnMs + MARGIN_MS);
      }),
      { numRuns: 15 },
    );
  });
});

describe("PBT: withTimeout — жёсткая стена (§5.2: «каждый внешний вызов имеет timeout»)", () => {
  it("fn, который никогда не завершается, — обёртка режется TimeoutError в границу", async () => {
    await fc.assert(
      fc.asyncProperty(slowTimeoutArb, async (timeoutMs) => {
        const startedAt = Date.now();
        await expect(
          withTimeout(() => new Promise<never>(() => {}), timeoutMs), // сигнал игнорируется
        ).rejects.toThrow(TimeoutError);
        expect(Date.now() - startedAt).toBeLessThan(timeoutMs + MARGIN_MS);
      }),
      { numRuns: 15 },
    );
  });

  it("fn, игнорирующий сигнал и завершающийся ПОЗЖЕ границы, — всё равно TimeoutError в границу", async () => {
    await fc.assert(
      fc.asyncProperty(slowTimeoutArb, fc.jsonValue({ maxDepth: 2 }), async (timeoutMs, lateValue) => {
        const startedAt = Date.now();
        await expect(
          withTimeout(
            () => sleep(timeoutMs * 4).then(() => lateValue), // сигнал не слушает
            timeoutMs,
          ),
        ).rejects.toThrow(TimeoutError);
        expect(Date.now() - startedAt).toBeLessThan(timeoutMs + MARGIN_MS);
      }),
      { numRuns: 10 },
    );
  });

  it("осиротевшая поздняя ОШИБКА fn не роняет процесс (unhandled rejection нет)", async () => {
    const unhandled: unknown[] = [];
    const onUnhandled = (reason: unknown): void => {
      unhandled.push(reason);
    };
    process.on("unhandledRejection", onUnhandled);
    try {
      const quickTimeoutArb = fc.integer({ min: 60, max: 150 });
      await fc.assert(
        fc.asyncProperty(quickTimeoutArb, async (timeoutMs) => {
          await expect(
            withTimeout(
              () =>
                sleep(timeoutMs * 3).then(() => {
                  throw new Error("поздняя ошибка осиротевшего fn");
                }),
              timeoutMs,
            ),
          ).rejects.toThrow(TimeoutError);
          // Ждём, пока осиротевший fn точно отклонится.
          await sleep(timeoutMs * 2 + 50);
        }),
        { numRuns: 4 },
      );
      expect(unhandled).toHaveLength(0); // гонка поглотила позднюю ошибку
    } finally {
      process.off("unhandledRejection", onUnhandled);
    }
  });

  it("успех до границы: обёртка не режет корректные вызовы", async () => {
    await fc.assert(
      fc.asyncProperty(slowTimeoutArb, fc.integer({ min: 3, max: 5 }), async (timeoutMs, factor) => {
        const fnMs = Math.floor(timeoutMs / factor);
        const result = await withTimeout(() => sleep(fnMs).then(() => "ok" as const), timeoutMs);
        expect(result).toBe("ok");
      }),
      { numRuns: 15 },
    );
  });
});

describe("PBT: withTimeout — сигнал отмены", () => {
  it("висящий fn, слушающий сигнал, завершается не позже границы (с запасом)", async () => {
    await fc.assert(
      fc.asyncProperty(slowTimeoutArb, async (timeoutMs) => {
        const startedAt = Date.now();
        const aborted = await withTimeout(
          (signal) =>
            new Promise<boolean>((resolve) => {
              signal.addEventListener("abort", () => resolve(signal.aborted), { once: true });
            }),
          timeoutMs,
        );
        const elapsed = Date.now() - startedAt;
        expect(aborted).toBe(true);
        expect(elapsed).toBeLessThan(timeoutMs + MARGIN_MS);
      }),
      { numRuns: 15 },
    );
  });

  it("причиной отмены сигнала является TimeoutError обёртки", async () => {
    await fc.assert(
      fc.asyncProperty(slowTimeoutArb, async (timeoutMs) => {
        const failure = await withTimeout(
          (signal) =>
            new Promise<never>((_resolve, reject) => {
              signal.addEventListener(
                "abort",
                () => reject(signal.reason instanceof Error ? signal.reason : new Error("иное")),
                { once: true },
              );
            }),
          timeoutMs,
        ).then(
          () => {
            throw new Error("ожидалось отклонение");
          },
          (error: unknown) => error,
        );
        expect(failure).toBeInstanceOf(TimeoutError);
      }),
      { numRuns: 15 },
    );
  });

  it("после раннего успеха сигнал не абортится задним числом (таймер снят)", async () => {
    await fc.assert(
      fc.asyncProperty(slowTimeoutArb, timeoutArb, async (fnMs, timeoutMs) => {
        fc.pre(fnMs < timeoutMs);
        let seen: AbortSignal | undefined;
        await withTimeout((signal) => {
          seen = signal;
          return sleep(fnMs).then(() => "ok" as const);
        }, timeoutMs);
        await sleep(timeoutMs + MARGIN_MS);
        expect(seen?.aborted).toBe(false);
      }),
      { numRuns: 10 },
    );
  });
});
