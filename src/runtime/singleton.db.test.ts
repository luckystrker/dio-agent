// Интеграционный тест (живой PostgreSQL, база dio_test).
// §5.1: процесс получает advisory lock; если lock занят — второй запуск
// завершается ошибкой SingletonTakenError; после release замок свободен.
// При потере singleton-соединения процесс обязан увидеть это (onLost).

import { afterAll, describe, expect, it } from "vitest";
import { acquireSingleton, SINGLETON_LOCK_KEY, SingletonTakenError } from "./singleton.js";
import { createTestPool, waitFor } from "../test-support/db.js";

const poolA = createTestPool(); // «первый процесс»
const poolB = createTestPool(); // «второй процесс»

afterAll(async () => {
  await Promise.allSettled([poolA.end(), poolB.end()]);
});

describe("acquireSingleton против реальной базы", () => {
  it("первый захват проходит, второй против той же базы — SingletonTakenError", async () => {
    const first = await acquireSingleton(poolA);
    try {
      await expect(acquireSingleton(poolB)).rejects.toThrow(SingletonTakenError);
      // Тот же пул (третье соединение) тоже не может занять session-lock.
      await expect(acquireSingleton(poolA)).rejects.toThrow(SingletonTakenError);
    } finally {
      await first.release();
    }
  });

  it("после release первого — замок снова захватывается другим процессом", async () => {
    const first = await acquireSingleton(poolA);
    await first.release();

    const second = await acquireSingleton(poolB);
    try {
      // Теперь уже первый не может: замок у второго процесса.
      await expect(acquireSingleton(poolA)).rejects.toThrow(SingletonTakenError);
    } finally {
      await second.release();
    }
  });

  it("release действительно освобождает session advisory lock (проверка из базы)", async () => {
    const holder = await acquireSingleton(poolA);
    // Пока замок удерживается, pg_try_advisory_lock из другого соединения невозможен.
    const probeDuring = await poolB.query<{ locked: boolean }>(
      "SELECT pg_try_advisory_lock($1) AS locked",
      [SINGLETON_LOCK_KEY],
    );
    expect(probeDuring.rows[0]?.locked).toBe(false);
    if (probeDuring.rows[0]?.locked) {
      await poolB.query("SELECT pg_advisory_unlock($1)", [SINGLETON_LOCK_KEY]);
    }
    await holder.release();

    const probeAfter = await poolB.query<{ locked: boolean }>(
      "SELECT pg_try_advisory_lock($1) AS locked",
      [SINGLETON_LOCK_KEY],
    );
    expect(probeAfter.rows[0]?.locked).toBe(true);
    await poolB.query("SELECT pg_advisory_unlock($1)", [SINGLETON_LOCK_KEY]);
  });

  it("onLost: обрыв singleton-соединения (pg_terminate_backend) вызывает колбэк", async () => {
    const singleton = await acquireSingleton(poolA);

    const lost: string[] = [];
    singleton.onLost((reason) => lost.push(reason));

    // Находим backend-процесс, держащий наш session advisory lock,
    // и убиваем его изнутри базы — соединение рвётся как при сбое сети/SIGKILL.
    const pids = await poolB.query<{ pid: number }>(
      `SELECT pid FROM pg_locks
       WHERE locktype = 'advisory' AND classid = 0 AND objid = $1 AND objsubid = 1`,
      [SINGLETON_LOCK_KEY],
    );
    expect(pids.rows).toHaveLength(1);
    await poolB.query("SELECT pg_terminate_backend($1)", [pids.rows[0]?.pid]);

    await waitFor(() => Promise.resolve(lost.length > 0), {
      timeoutMs: 5_000,
      label: "колбэк onLost сработал после обрыва соединения",
    });
    expect(lost[0]).toBeTruthy();

    // Мёртвое соединение не держит замок: другой процесс может захватить.
    const next = await acquireSingleton(poolB);
    await next.release();

    // Возвращаем убитое соединение в пул (иначе pool.end() повиснет на checkout).
    await singleton.release().catch(() => undefined);
  });
});
