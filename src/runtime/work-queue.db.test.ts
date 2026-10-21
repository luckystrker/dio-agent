// Интеграционный тест (живой PostgreSQL, база dio_test).
// §5.2: строгий порядок приоритетов direct > group > intention >
// exploration > consolidation; внутри приоритета — FIFO; захваченная
// работа не отдаётся повторно. §12.2/§5.3: recoverStale возвращает
// незавершённую работу в pending либо помечает терминальный пропуск.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { WorkQueueRepo } from "./work-queue.js";
import { WORK_MAX_ATTEMPTS, WORK_PRIORITIES } from "./priorities.js";
import { cleanTables, createTestPool, getWorkItem } from "../test-support/db.js";
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

describe("claimNext: порядок приоритетов и FIFO", () => {
  it("работа отдаётся строго по приоритетам, внутри приоритета — FIFO по порядку вставки", async () => {
    // Раскладываем в обратном порядке, чтобы поймать сортировку по created_at/id,
    // а не по порядку вставки.
    const consolidation = await repo.enqueue("consolidation", { n: "c1" });
    const exploration1 = await repo.enqueue("exploration", { n: "e1" });
    const direct1 = await repo.enqueue("direct", { n: "d1" });
    const intention1 = await repo.enqueue("intention", { n: "i1" });
    const group1 = await repo.enqueue("group", { n: "g1" });
    const direct2 = await repo.enqueue("direct", { n: "d2" });
    const exploration2 = await repo.enqueue("exploration", { n: "e2" });
    const group2 = await repo.enqueue("group", { n: "g2" });

    // Claim идёт по одному, и мы напрямую сверяем (kind, id) каждой выдачи.
    const claimed: Array<{ kind: string; id: string }> = [];
    for (let i = 0; i < 8; i++) {
      const item = await repo.claimNext();
      expect(item).not.toBeNull();
      claimed.push({ kind: item!.kind, id: item!.id });
    }
    expect(await repo.claimNext()).toBeNull();

    expect(claimed).toEqual([
      { kind: "direct", id: direct1 }, // 50, вставлен раньше direct2
      { kind: "direct", id: direct2 }, // 50
      { kind: "group", id: group1 }, // 40
      { kind: "group", id: group2 }, // 40
      { kind: "intention", id: intention1 }, // 30
      { kind: "exploration", id: exploration1 }, // 20, раньше exploration2
      { kind: "exploration", id: exploration2 }, // 20
      { kind: "consolidation", id: consolidation }, // 10
    ]);
  });

  it("FIFO не зависит от приоритетного перемешивания: поздний direct позже раннего direct", async () => {
    const early = await repo.enqueue("direct", { n: "ранний" });
    await repo.enqueue("consolidation", {}); // ниже приоритетом — не лезет вперёд
    await repo.enqueue("group", {});
    const late = await repo.enqueue("direct", { n: "поздний" });

    const first = await repo.claimNext();
    const second = await repo.claimNext();
    expect(first?.id).toBe(early);
    expect(second?.id).toBe(late);
    expect(Number(second!.id)).toBeGreaterThan(Number(first!.id)); // порядок вставки, не id-совпадение
  });

  it("enqueue с явным priority переопределяет приоритет вида работы", async () => {
    // consolidation по умолчанию 10, но явный приоритет поднимает его над direct (50).
    const boosted = await repo.enqueue("consolidation", {}, 90);
    const direct = await repo.enqueue("direct", {});
    const row = await getWorkItem(pool, boosted);
    expect(row?.priority).toBe(90);

    const first = await repo.claimNext();
    expect(first?.id).toBe(boosted);
    const second = await repo.claimNext();
    expect(second?.id).toBe(direct);
  });

  it("конкурентный claim из двух пулов: каждая работа выдаётся ровно один раз", async () => {
    const otherPool = createTestPool();
    const otherRepo = new WorkQueueRepo(otherPool);
    try {
      const ids: string[] = [];
      for (let i = 0; i < 12; i++) {
        ids.push(await repo.enqueue(i % 2 === 0 ? "direct" : "group", { i }));
      }

      // Два «процесса» дерутся за одну очередь параллельными claim.
      const claimedByBoth: string[] = [];
      await Promise.all(
        Array.from({ length: 12 }, () =>
          (Math.random() < 0.5 ? repo : otherRepo)
            .claimNext()
            .then((item) => {
              if (item) claimedByBoth.push(item.id);
            }),
        ),
      );

      // Ни потерь, ни дублей: собраны все 12, каждая ровно один раз.
      expect(claimedByBoth).toHaveLength(12);
      expect(new Set(claimedByBoth).size).toBe(12);
      expect([...claimedByBoth].sort((a, b) => Number(a) - Number(b))).toEqual(
        [...ids].sort((a, b) => Number(a) - Number(b)),
      );
      expect(await repo.claimNext()).toBeNull();
      expect(await otherRepo.claimNext()).toBeNull();
    } finally {
      await otherPool.end();
    }
  });

  it("захваченная работа переводится в running с attempts=1 и не отдаётся повторно", async () => {
    const id = await repo.enqueue("direct", { n: 1 });
    const item = await repo.claimNext();
    expect(item).not.toBeNull();
    expect(item!.id).toBe(id);
    expect(item!.status).toBe("running");
    expect(item!.attempts).toBe(1);

    const row = await getWorkItem(pool, id);
    expect(row?.status).toBe("running");
    expect(row?.attempts).toBe(1);

    // Единственная работа в running — повторный claim ничего не даёт.
    expect(await repo.claimNext()).toBeNull();
    expect(await repo.pendingCount()).toBe(0);
  });

  it("enqueue без явного приоритета берёт приоритет вида работы", async () => {
    for (const kind of Object.keys(WORK_PRIORITIES) as Array<keyof typeof WORK_PRIORITIES>) {
      const id = await repo.enqueue(kind, {});
      const row = await getWorkItem(pool, id);
      expect(row?.priority).toBe(WORK_PRIORITIES[kind]);
    }
  });

  it("requeue возвращает работу в pending, следующий claim снова её отдаёт с attempts+1", async () => {
    const id = await repo.enqueue("group", {});
    const first = await repo.claimNext();
    expect(first?.id).toBe(id);

    await repo.requeue(id);
    expect(await repo.pendingCount()).toBe(1);

    const second = await repo.claimNext();
    expect(second?.id).toBe(id);
    expect(second?.attempts).toBe(2);
  });

  it("skip фиксирует терминальный пропуск с причиной и работа больше не отдаётся", async () => {
    const id = await repo.enqueue("intention", {});
    await repo.skip(id, "осознанный отказ");
    const row = await getWorkItem(pool, id);
    expect(row?.status).toBe("skipped");
    expect(row?.skip_reason).toBe("осознанный отказ");
    expect(await repo.claimNext()).toBeNull();
  });

  it("complete помечает done и работа покидает очередь", async () => {
    const id = await repo.enqueue("direct", {});
    const item = await repo.claimNext();
    await repo.complete(item!.id);
    const row = await getWorkItem(pool, id);
    expect(row?.status).toBe("done");
    expect(await repo.claimNext()).toBeNull();
  });
});

describe("recoverStale: восстановление после падения процесса", () => {
  it("running с attempts < WORK_MAX_ATTEMPTS возвращается в pending без сброса attempts", async () => {
    const id = await repo.enqueue("direct", {});
    await pool.query("UPDATE work_items SET status = 'running', attempts = 1 WHERE id = $1", [id]);

    const result = await repo.recoverStale();
    expect(result).toEqual({ requeued: 1, skipped: 0 });

    const row = await getWorkItem(pool, id);
    expect(row?.status).toBe("pending");
    expect(row?.attempts).toBe(1); // попытка сорвавшегося claim сохраняется
  });

  it("running с attempts >= WORK_MAX_ATTEMPTS — терминальный skipped с причиной (граница)", async () => {
    const id = await repo.enqueue("exploration", {});
    await pool.query("UPDATE work_items SET status = 'running', attempts = $2 WHERE id = $1", [
      id,
      WORK_MAX_ATTEMPTS,
    ]);

    const result = await repo.recoverStale();
    expect(result).toEqual({ requeued: 0, skipped: 1 });

    const row = await getWorkItem(pool, id);
    expect(row?.status).toBe("skipped");
    expect(row?.skip_reason).toBeTruthy();
    expect(await repo.claimNext()).toBeNull();
  });

  it("после recovery pending-работа снова захватывается, attempts продолжают расти", async () => {
    const id = await repo.enqueue("group", {});
    await pool.query("UPDATE work_items SET status = 'running', attempts = 1 WHERE id = $1", [id]);
    await repo.recoverStale();

    const reclaimed = await repo.claimNext();
    expect(reclaimed?.id).toBe(id);
    expect(reclaimed?.attempts).toBe(2);
  });

  it("несколько running-работ обрабатываются все; посторонние статусы не трогаются", async () => {
    const requeue1 = await repo.enqueue("direct", {});
    const requeue2 = await repo.enqueue("group", {});
    const skip1 = await repo.enqueue("exploration", {});
    const skip2 = await repo.enqueue("consolidation", {});
    const done = await repo.enqueue("intention", {});
    const pendingUntouched = await repo.enqueue("exploration", {});

    await pool.query("UPDATE work_items SET status = 'running', attempts = 1 WHERE id = ANY($1)", [
      [requeue1, requeue2],
    ]);
    await pool.query(
      "UPDATE work_items SET status = 'running', attempts = $2 WHERE id = ANY($1)",
      [[skip1, skip2], WORK_MAX_ATTEMPTS],
    );
    await pool.query("UPDATE work_items SET status = 'done' WHERE id = $1", [done]);

    const result = await repo.recoverStale();
    expect(result).toEqual({ requeued: 2, skipped: 2 });

    expect((await getWorkItem(pool, requeue1))?.status).toBe("pending");
    expect((await getWorkItem(pool, requeue2))?.status).toBe("pending");
    expect((await getWorkItem(pool, skip1))?.status).toBe("skipped");
    expect((await getWorkItem(pool, skip2))?.status).toBe("skipped");
    expect((await getWorkItem(pool, done))?.status).toBe("done"); // не тронут
    expect((await getWorkItem(pool, pendingUntouched))?.status).toBe("pending"); // не тронут

    // Ни одной работы не осталось «вечно в running».
    const running = await pool.query("SELECT count(*)::int AS n FROM work_items WHERE status = 'running'");
    expect(running.rows[0]?.n).toBe(0);
  });

  it("без running-работ recoverStale ничего не делает", async () => {
    await repo.enqueue("direct", {});
    expect(await repo.recoverStale()).toEqual({ requeued: 0, skipped: 0 });
    expect(await repo.pendingCount()).toBe(1);
  });
});

// Этап 6.5: наблюдаемость §20 — глубина очереди и возраст старейшей
// pending-работы (находка ревью: pendingCount написан, но возраста не было).
describe("queueStats: глубина и возраст очереди (§20)", () => {
  it("пустая очередь: pending 0, возраст null", async () => {
    expect(await repo.queueStats()).toEqual({ pending: 0, oldestPendingAgeSec: null });
  });

  it("свежая и состаренная работа: возраст старейшей считается от created_at", async () => {
    await repo.enqueue("direct", {});
    const old = await repo.enqueue("consolidation", {});
    await pool.query("UPDATE work_items SET created_at = now() - interval '300 seconds' WHERE id = $1", [old]);

    const stats = await repo.queueStats();
    expect(stats.pending).toBe(2);
    expect(stats.oldestPendingAgeSec).not.toBeNull();
    expect(stats.oldestPendingAgeSec!).toBeGreaterThanOrEqual(299);
  });

  it("не-pending работы в глубину и возраст не попадают", async () => {
    const id = await repo.enqueue("direct", {});
    const claimed = await repo.claimNext(); // direct → running
    expect(claimed).not.toBeNull();
    expect(claimed!.id).toBe(id);
    await pool.query("UPDATE work_items SET created_at = now() - interval '300 seconds' WHERE id = $1", [
      claimed!.id,
    ]);
    expect(await repo.queueStats()).toEqual({ pending: 0, oldestPendingAgeSec: null });
  });
});
