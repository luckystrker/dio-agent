import type { Pool, PoolClient } from "pg";

// Ключ advisory-замка константой на всё приложение: "dio" в hex.
export const SINGLETON_LOCK_KEY = 0x64_69_6f;

export class SingletonTakenError extends Error {
  constructor() {
    super("Singleton-замок занят: другой экземпляр процесса уже работает с этой базой.");
    this.name = "SingletonTakenError";
  }
}

export interface Singleton {
  /** Освободить замок и вернуть соединение. Вызывать при shutdown. */
  release(): Promise<void>;
  /**
   * Реакция на потерю singleton-соединения. Без живого замка продолжать
   * работу нельзя (ARCHITECTURE §5.1) — вызывающий обязан инициировать shutdown.
   */
  onLost(callback: (reason: string) => void): void;
}

export async function acquireSingleton(pool: Pool): Promise<Singleton> {
  const client: PoolClient = await pool.connect();

  const result = await client
    .query<{ locked: boolean }>("SELECT pg_try_advisory_lock($1) AS locked", [SINGLETON_LOCK_KEY])
    .catch(async (error: unknown) => {
      client.release();
      throw error;
    });

  if (!result.rows[0]?.locked) {
    client.release();
    throw new SingletonTakenError();
  }

  let lostCallback: ((reason: string) => void) | undefined;
  client.on("error", (error: Error) => {
    lostCallback?.(error instanceof Error ? error.message : String(error));
  });
  client.on("end", () => {
    lostCallback?.("singleton-соединение закрыто");
  });

  return {
    async release(): Promise<void> {
      try {
        await client.query("SELECT pg_advisory_unlock($1)", [SINGLETON_LOCK_KEY]);
      } finally {
        client.release(true);
      }
    },
    onLost(callback: (reason: string) => void): void {
      lostCallback = callback;
    },
  };
}
