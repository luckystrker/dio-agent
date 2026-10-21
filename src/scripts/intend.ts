// Создать намерение вручную — для живой приёмки этапа 5
// («персонаж сам возвращается к теме»): npm run intend -- --chat 111777 --topic "велопрогулки" --in 10s [--kind check_in]

import "dotenv/config";
import { createDatabasePool } from "../runtime/db.js";
import { withTransaction } from "../runtime/transactions.js";
import { IntentionRepo } from "../background/intentions.js";
import type { IntentionKind } from "../background/intention-proposals.js";

function parseArgs(): { chat: string; topic: string; kind: IntentionKind; delayHours: number } {
  const args = process.argv.slice(2);
  const value = (flag: string): string | undefined => {
    const index = args.indexOf(flag);
    return index === -1 ? undefined : args[index + 1];
  };
  const chat = value("--chat");
  const topic = value("--topic");
  const kindRaw = value("--kind") ?? "check_in";
  const inRaw = value("--in") ?? "10s";
  if (!chat || !topic) {
    console.error('Использование: npm run intend -- --chat <id> --topic "тема" [--kind check_in|reply_later|share|explore] [--in 10s|2h|0.5h]');
    process.exit(1);
  }
  const match = /^(\d+(?:\.\d+)?)\s*([smh])$/.exec(inRaw);
  if (!match) {
    console.error(`--in не разобран: "${inRaw}" (формат: 30s, 10m, 2h)`);
    process.exit(1);
  }
  const amount = Number(match[1]);
  const unit = match[2];
  const delayHours = unit === "h" ? amount : unit === "m" ? amount / 60 : amount / 3600;
  const kinds: IntentionKind[] = ["reply_later", "check_in", "share", "explore"];
  if (!kinds.includes(kindRaw as IntentionKind)) {
    console.error(`--kind "${kindRaw}" не поддерживается: ${kinds.join(", ")}`);
    process.exit(1);
  }
  return { chat, topic, kind: kindRaw as IntentionKind, delayHours };
}

const args = parseArgs();
const databaseUrl = process.env["DATABASE_URL"];
if (!databaseUrl) {
  console.error("DATABASE_URL не задан (см. .env.example).");
  process.exit(1);
}

const pool = createDatabasePool(databaseUrl);
try {
  const repo = new IntentionRepo(pool);
  const created = await withTransaction(pool, (client) =>
    repo.create(client, {
      kind: args.kind,
      topic: args.topic,
      reason: "создано вручную (npm run intend)",
      targetChatId: args.chat,
      delayHours: args.delayHours,
    }),
  );
  if (created === null) {
    console.log("Дубликат (dedupe §14.1) — намерение уже существует.");
  } else {
    console.log(
      `Намерение #${created.id}: ${created.kind} «${created.topic}» чат ${created.targetChatId}, срок через ${args.delayHours.toFixed(4)} ч.`,
    );
  }
} finally {
  await pool.end();
}
