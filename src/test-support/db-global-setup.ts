// Глобальная подготовка интеграционных тестов: отдельная база dio_test
// (разработческая база dio не трогается) + миграции из migrations/.
// Идемпотентно: повторный прогон использует существующую базу и
// уже применённые миграции (node-pg-migrate хранит pgmigrations).

import { Client } from "pg";
import { join } from "node:path";
import { runner } from "node-pg-migrate";
import { TEST_DATABASE_URL } from "./db.js";

export default async function setup(): Promise<() => Promise<void>> {
  await ensureDatabaseExists();
  const applied = await runner({
    databaseUrl: TEST_DATABASE_URL,
    dir: join(import.meta.dirname, "..", "..", "migrations"),
    migrationsTable: "pgmigrations",
    direction: "up",
  });
  console.log(`[db-global-setup] миграций применено: ${applied.length} (${TEST_DATABASE_URL})`);

  return async () => {
    // Базу не дропаем: следующий прогон начинается быстрее и проверяет
    // идемпотентность на реальном состоянии.
  };
}

async function ensureDatabaseExists(): Promise<void> {
  const adminUrl = new URL(TEST_DATABASE_URL);
  const databaseName = adminUrl.pathname.replaceAll("/", "");
  if (!databaseName) {
    throw new Error(`не удалось извлечь имя базы из ${TEST_DATABASE_URL}`);
  }
  adminUrl.pathname = "/postgres";

  const admin = new Client({ connectionString: adminUrl.toString() });
  await admin.connect();
  try {
    const existing = await admin.query<{ exists: boolean }>(
      "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1) AS exists",
      [databaseName],
    );
    if (!existing.rows[0]?.exists) {
      // Имя базы подставляем только после проверки белого списка символов.
      if (!/^[a-z_][a-z0-9_]*$/.test(databaseName)) {
        throw new Error(`недопустимое имя тестовой базы: ${databaseName}`);
      }
      await admin.query(`CREATE DATABASE "${databaseName}"`);
      console.log(`[db-global-setup] создана база ${databaseName}`);
    }
  } finally {
    await admin.end();
  }
}
