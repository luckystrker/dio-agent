// Программный раннер миграций node-pg-migrate.
// Использование: npm run migrate (применить все), npm run migrate:create <имя> (создать новую).

import "dotenv/config";
import { join } from "node:path";
import { runner } from "node-pg-migrate";

const databaseUrl = process.env["DATABASE_URL"];
if (!databaseUrl) {
  console.error("DATABASE_URL не задан. Скопируй .env.example в .env и заполни.");
  process.exit(1);
}

const migrationsDir = join(import.meta.dirname, "..", "..", "migrations");

try {
  const applied = await runner({
    databaseUrl,
    dir: migrationsDir,
    migrationsTable: "pgmigrations",
    direction: "up",
    verbose: true,
  });
  console.log(`Миграции применены: ${applied.length}`);
} catch (error) {
  console.error("Миграции не применены:", error instanceof Error ? error.message : error);
  process.exitCode = 1;
}
