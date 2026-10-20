// Интеграционный тест (живой PostgreSQL, база dio_test).
// §5.2/§12: единственный worker выполняет работы по приоритету;
// ошибка обработчика при неисчерпанных попытках → requeue, при
// исчерпании → терминальный skipped; SkipWorkError → skipped без
// повторов; таймаут обработчика — через AbortSignal.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { CognitiveWorker, SkipWorkError, type WorkHandler } from "./worker.js";
import { WorkQueueRepo } from "./work-queue.js";
import { WORK_MAX_ATTEMPTS } from "./priorities.js";
import { createLogger } from "./logger.js";
import { cleanTables, createTestPool, getWorkItem, sleep, waitFor } from "../test-support/db.js";
import type { Pool } from "pg";

let pool: Pool;
let repo: WorkQueueRepo;

beforeEach(async () => {
  pool = createTestPool();
  repo = new WorkQueueRepo(pool);
  await cleanTables(pool);
});

afterEach(async () => {
  await pool.end();
});

const logger = createLogger("error"); // тихий: шум worker'а не нужен в выводе теста

async function statusOf(id: string): Promise<string | undefined> {
  return (await getWorkItem(pool, id))?.status;
}

describe("CognitiveWorker с реальной БД", () => {
  it("выполняет работы строго по приоритету, завершая каждую done", async () => {
    // Ставим в очередь ДО старта worker'а: порядок определяет claim, не гонка.
    await repo.enqueue("consolidation", { n: "c" });
    await repo.enqueue("exploration", { n: "e" });
    await repo.enqueue("group", { n: "g" });
    await repo.enqueue("intention", { n: "i" });
    await repo.enqueue("direct", { n: "d" });

    const processed: string[] = [];
    const handler: WorkHandler = async (item) => {
      processed.push(item.kind);
    };
    const worker = new CognitiveWorker(repo, { direct: handler, group: handler, intention: handler, exploration: handler, consolidation: handler }, logger, {
      pollIntervalMs: 20,
    });
    worker.start();
    try {
      // Детерминированно: ждём, пока ВСЕ работы получат терминальный done
      // (pendingCount обнуляется ещё до complete последней работы).
      await waitFor(
        async () => {
          const rows = await pool.query<{ status: string }>("SELECT status FROM work_items");
          return rows.rows.length === 5 && rows.rows.every((row) => row.status === "done");
        },
        { label: "все пять работ получили статус done" },
      );
    } finally {
      await worker.stop();
    }

    expect(processed).toEqual(["direct", "group", "intention", "exploration", "consolidation"]);

    const rows = await pool.query<{ status: string; kind: string }>(
      "SELECT status, kind FROM work_items",
    );
    expect(rows.rows).toHaveLength(5);
    expect(rows.rows.every((row) => row.status === "done")).toBe(true);
  });

  it("ошибка обработчика при неисчерпанных попытках → requeue, затем повторный claim", async () => {
    let calls = 0;
    const handler: WorkHandler = async () => {
      calls++;
      if (calls === 1) throw new Error("временная ошибка");
      // вторая попытка — успех
    };
    const worker = new CognitiveWorker(repo, { direct: handler }, logger, { pollIntervalMs: 20 });
    const id = await repo.enqueue("direct", {});

    worker.start();
    try {
      await waitFor(async () => (await statusOf(id)) === "done", { label: "работа доезжает со второй попытки" });
    } finally {
      await worker.stop();
    }

    expect(calls).toBe(2);
    const row = await getWorkItem(pool, id);
    expect(row?.status).toBe("done");
    expect(row?.attempts).toBe(2); // обе попытки посчитаны в claim
  });

  it("ошибка при исчерпании попыток → терминальный skipped с причиной", async () => {
    let calls = 0;
    const handler: WorkHandler = async () => {
      calls++;
      throw new Error("всегда падаю");
    };
    const worker = new CognitiveWorker(repo, { direct: handler }, logger, { pollIntervalMs: 20 });
    const id = await repo.enqueue("direct", {});

    worker.start();
    try {
      await waitFor(async () => (await statusOf(id)) === "skipped", { label: "работа помечена пропуском" });
    } finally {
      await worker.stop();
    }

    expect(calls).toBe(WORK_MAX_ATTEMPTS); // ровно лимит попыток, не больше
    const row = await getWorkItem(pool, id);
    expect(row?.status).toBe("skipped");
    expect(row?.attempts).toBe(WORK_MAX_ATTEMPTS);
    expect(row?.skip_reason).toMatch(/превышен лимит попыток.*всегда падаю/);
    expect(await repo.pendingCount()).toBe(0); // из очереди ушла окончательно
  });

  it("SkipWorkError → skipped без повторов, причина сохранена", async () => {
    let calls = 0;
    const handler: WorkHandler = async () => {
      calls++;
      throw new SkipWorkError("осознанный отказ", "причина отказа X");
    };
    const worker = new CognitiveWorker(repo, { group: handler }, logger, { pollIntervalMs: 20 });
    const id = await repo.enqueue("group", {});

    worker.start();
    try {
      await waitFor(async () => (await statusOf(id)) === "skipped", { label: "пропуск зафиксирован" });
      await sleep(120); // убеждаемся, что повторов нет
    } finally {
      await worker.stop();
    }

    expect(calls).toBe(1); // ни одного повтора
    const row = await getWorkItem(pool, id);
    expect(row?.status).toBe("skipped");
    expect(row?.attempts).toBe(1);
    expect(row?.skip_reason).toBe("причина отказа X");
  });

  it("таймаут обработчика: сигнал отмены приходит в пределах границы", async () => {
    const handlerTimeoutMs = 120;
    let abortObservedAt: number | undefined;
    const handler: WorkHandler = (item, signal) =>
      new Promise<void>((_resolve, reject) => {
        void item;
        signal.addEventListener(
          "abort",
          () => {
            abortObservedAt = Date.now();
            reject(new Error("отменён по таймауту"));
          },
          { once: true },
        );
      });

    const worker = new CognitiveWorker(repo, { exploration: handler }, logger, {
      pollIntervalMs: 20,
      handlerTimeoutMs,
    });
    const id = await repo.enqueue("exploration", {});
    const startedAt = Date.now();

    worker.start();
    try {
      await waitFor(async () => (await statusOf(id)) === "skipped", {
        timeoutMs: 20_000,
        label: "исчерпание попыток после таймаутов",
      });
    } finally {
      await worker.stop();
    }

    expect(abortObservedAt).toBeDefined();
    expect(abortObservedAt! - startedAt).toBeLessThan(handlerTimeoutMs + 500);
    const row = await getWorkItem(pool, id);
    expect(row?.status).toBe("skipped");
    expect(row?.attempts).toBe(WORK_MAX_ATTEMPTS);
  });

  it("worker.stop() дожидается текущей работы: обработчик доезжает, работа done (§5.3)", async () => {
    let finishHandler: (() => void) | undefined;
    let handlerDone = false;
    const handler: WorkHandler = () =>
      new Promise<void>((resolve) => {
        finishHandler = resolve; // завершаем только извне
      }).then(() => {
        handlerDone = true;
      });
    const worker = new CognitiveWorker(repo, { direct: handler }, logger, { pollIntervalMs: 20 });
    const id = await repo.enqueue("direct", {});

    worker.start();
    try {
      // Ждём, пока worker возьмёт работу и обработчик останется висеть.
      await waitFor(
        () => Promise.resolve(finishHandler !== undefined),
        { label: "worker захватил работу, обработчик в полёте" },
      );
      const inFlight = await getWorkItem(pool, id);
      expect(inFlight?.status).toBe("running");

      const stopped = worker.stop(); // остановка ДО завершения обработчика
      expect(handlerDone).toBe(false); // работа ещё на руках

      finishHandler!(); // отпускаем обработчик
      await stopped; // stop дожидается текущей операции
    } finally {
      finishHandler?.();
      await worker.stop();
    }

    expect(handlerDone).toBe(true);
    const row = await getWorkItem(pool, id);
    expect(row?.status).toBe("done");
  });

  it("работа без обработчика — терминальный пропуск, а не вечный цикл", async () => {
    const worker = new CognitiveWorker(repo, {}, logger, { pollIntervalMs: 20 });
    const id = await repo.enqueue("consolidation", {});

    worker.start();
    try {
      await waitFor(async () => (await statusOf(id)) === "skipped", { label: "пропуск без обработчика" });
    } finally {
      await worker.stop();
    }

    const row = await getWorkItem(pool, id);
    expect(row?.skip_reason).toMatch(/нет обработчика/);
    expect(row?.attempts).toBe(1);
  });
});
