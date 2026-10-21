// Пауза повторов (§12.1, чек-лист 1.8): работа возвращается в очередь
// с next_attempt_at; захват учитывает срок. Retry без задержки сжигает
// лимит попыток при кратковременном сбое провайдера.

export const up = (pgm) => {
  pgm.addColumn("work_items", {
    next_attempt_at: { type: "timestamptz" },
  });
  pgm.createIndex("work_items", ["status", "next_attempt_at"]);
};

export const down = (pgm) => {
  pgm.dropIndex("work_items", ["status", "next_attempt_at"]);
  pgm.dropColumn("work_items", "next_attempt_at");
};
