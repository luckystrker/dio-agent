// Постановка «прямого обращения» в очередь вручную — для дымовых прогонов
// и ручной приёмки этапа 1: npm run turn -- "привет, как дела?"

import "dotenv/config";
import { createDatabasePool } from "../runtime/db.js";
import { WorkQueueRepo } from "../runtime/work-queue.js";

const text = process.argv.slice(2).join(" ").trim();
if (!text) {
  console.error("Использование: npm run turn -- \"текст сообщения\"");
  process.exit(1);
}

const databaseUrl = process.env["DATABASE_URL"];
if (!databaseUrl) {
  console.error("DATABASE_URL не задан (см. .env.example).");
  process.exit(1);
}

const pool = createDatabasePool(databaseUrl);
try {
  const repo = new WorkQueueRepo(pool);
  const id = await repo.enqueue("direct", { messages: [{ role: "user", content: text }] });
  console.log(`Поставлено в очередь: work_items.id=${id} (kind=direct)`);
} finally {
  await pool.end();
}
