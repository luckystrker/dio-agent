import { Pool } from "pg";

// Один пул на процесс. База — источник истины (PHILOSOPHY §2);
// память процесса — кэш, теряемый без последствий.
export function createDatabasePool(databaseUrl: string): Pool {
  return new Pool({
    connectionString: databaseUrl,
    max: 5,
    idleTimeoutMillis: 30_000,
    connectionTimeoutMillis: 5_000,
  });
}
