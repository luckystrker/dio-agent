// Фон (этап 5, §14–§16): единая модель намерений с dedupe и сроками,
// журнал проактивных отправок для политики §14.3.

export const up = (pgm) => {
  pgm.createTable("intentions", {
    id: { type: "bigserial", primaryKey: true },
    kind: { type: "text", notNull: true }, // reply_later | check_in | share | explore
    target_chat_id: { type: "bigint", notNull: true },
    topic: { type: "text", notNull: true },
    entity_ids: { type: "jsonb", notNull: true, default: "[]" },
    reason: { type: "text", notNull: true },
    // pending | queued | running | completed | cancelled | failed (§14.1)
    status: { type: "text", notNull: true, default: "pending" },
    not_before: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
    expires_at: { type: "timestamptz" },
    // §14.1: dedupe по типу+теме+чату — перефразировки не размножают поиск
    dedupe_key: { type: "text", notNull: true, unique: true },
    related_turn_id: { type: "bigint" },
    created_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
    updated_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.createIndex("intentions", ["status", "not_before"]);

  // Журнал инициативных отправок (§14.3: ≤2/день на личный чат, ≥4ч между)
  pgm.createTable("proactive_log", {
    id: { type: "bigserial", primaryKey: true },
    chat_id: { type: "bigint", notNull: true },
    intention_id: { type: "bigint", references: { schema: "public", name: "intentions" } },
    sent_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.createIndex("proactive_log", ["chat_id", "sent_at"]);
};

export const down = (pgm) => {
  pgm.dropTable("proactive_log");
  pgm.dropTable("intentions");
};
