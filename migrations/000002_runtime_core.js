// Ядро runtime (этап 1): очередь работы и журнал состояния.
// Полная схема §18 (social_events, turns, память) — этапы 2–3.

export const up = (pgm) => {
  pgm.createTable("work_items", {
    id: { type: "bigserial", primaryKey: true },
    kind: { type: "text", notNull: true },
    priority: { type: "integer", notNull: true },
    payload: { type: "jsonb", notNull: true, default: "{}" },
    // pending | running | done | skipped (терминальный пропуск фиксируется явно)
    status: { type: "text", notNull: true, default: "pending" },
    attempts: { type: "integer", notNull: true, default: 0 },
    skip_reason: { type: "text" },
    created_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
    updated_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.createIndex("work_items", ["status", "priority", "created_at"]);

  pgm.createTable("state_notes", {
    id: { type: "bigserial", primaryKey: true },
    work_item_id: { type: "bigint", references: { schema: "public", name: "work_items" } },
    text: { type: "text", notNull: true },
    created_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
};

export const down = (pgm) => {
  pgm.dropTable("state_notes");
  pgm.dropTable("work_items");
};
