// Покормить бота в dry-run режиме: дописывает Telegram-update строки в файл,
// который читает DryRunTelegramApi работающего процесса.
// Использование: npm run feed -- "привет" "как дела" [--group] [--reply]

import { appendFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

const args = process.argv.slice(2);
const isGroup = args.includes("--group");
const isReply = args.includes("--reply");
const texts = args.filter((arg) => !arg.startsWith("--"));
if (texts.length === 0) {
  console.error('Использование: npm run feed -- "сообщение" ["ещё"] [--group] [--reply]');
  process.exit(1);
}

const UPDATES_FILE = fileURLToPath(new URL("../../.dry-run-updates.jsonl", import.meta.url));

// база update_id — от текущего времени, чтобы не конфликтовать с прошлыми прогонами
let updateId = Math.floor(Date.now() / 1000);
const chatId = isGroup ? -100777 : 111777;
const lines = texts.map((text) =>
  JSON.stringify({
    update_id: updateId++,
    message: {
      message_id: updateId,
      from: { id: 4242, is_bot: false },
      chat: { id: chatId, type: isGroup ? "supergroup" : "private" },
      date: Math.floor(Date.now() / 1000),
      ...(isReply ? { reply_to_message: { message_id: 1, chat: { id: chatId } } } : {}),
      text,
    },
  }),
);

try {
  await writeFile(UPDATES_FILE, "", { flag: "a" }); // создать, если нет
  await appendFile(UPDATES_FILE, lines.map((line) => `${line}\n`).join(""));
  console.log(`Записано обновлений: ${lines.length} (чат ${chatId}${isGroup ? ", группа" : ", личка"}${isReply ? ", reply" : ""}) → ${UPDATES_FILE}`);
} catch (error) {
  console.error("Не удалось записать:", error instanceof Error ? error.message : String(error));
  process.exit(1);
}
