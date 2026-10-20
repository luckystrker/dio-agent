// Бюджеты в БД (§16): резервирование с уникальностью операции —
// перезапуски процесса не размножают дневной лимит.

export const up = (pgm) => {
  pgm.createTable("budget_reservations", {
    id: { type: "bigserial", primaryKey: true },
    operation_id: { type: "text", notNull: true },
    day: { type: "date", notNull: true },
    created_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.createIndex("budget_reservations", ["operation_id", "day"], { unique: true });
  pgm.createIndex("budget_reservations", ["day"]);
};

export const down = (pgm) => {
  pgm.dropTable("budget_reservations");
};
