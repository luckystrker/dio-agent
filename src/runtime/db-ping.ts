import { Client } from "pg";
import type { Logger } from "./logger.js";

// Проверка связи с PostgreSQL. На этапе 0 результат не фатален:
// полноценный startup по §5.3 (подключить БД → singleton → recovery) появится на этапе 1.
export async function pingDatabase(databaseUrl: string, logger: Logger): Promise<boolean> {
  const client = new Client({
    connectionString: databaseUrl,
    connectionTimeoutMillis: 3000,
  });
  try {
    await client.connect();
    const result = await client.query<{ version: string }>("SELECT version()");
    const version = result.rows[0]?.["version"] ?? "unknown";
    logger.debug("пинг БД прошёл", { version: version.split(" ").slice(0, 2).join(" ") });
    return true;
  } catch (error) {
    logger.warn("пинг БД не прошёл (этап 0: продолжаю без БД)", {
      error: error instanceof Error ? error.message : String(error),
    });
    return false;
  } finally {
    await client.end().catch(() => undefined);
  }
}
