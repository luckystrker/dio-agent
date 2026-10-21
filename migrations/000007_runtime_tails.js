// Хвосты ревью Harness (чек-лист 1.7, 2.7):
// 1.7 — второй пояс идемпотентности применения: UNIQUE(work_item_id, op_index)
//       на state_notes; закрытие окна «commit применения → complete работы».
// 2.7 — validated_result у turns: результат хода переживает процесс и
//       применяется без нового вызова LLM (§12.2).

export const up = (pgm) => {
  pgm.addColumn("state_notes", {
    op_index: { type: "integer", notNull: true, default: 0 },
  });
  pgm.createIndex("state_notes", ["work_item_id", "op_index"], { unique: true });

  pgm.addColumn("turns", {
    validated_result: { type: "jsonb" },
  });
};

export const down = (pgm) => {
  pgm.dropColumn("turns", "validated_result");
  pgm.dropIndex("state_notes", ["work_item_id", "op_index"]);
  pgm.dropColumn("state_notes", "op_index");
};
