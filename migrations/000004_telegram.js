// Telegram-слой (этап 2): журнал событий, offset polling, turns и outbox.
// Соответствует §6, §12.2, §13.1, §18 спецификации.

export const up = (pgm) => {
  pgm.createTable("social_events", {
    id: { type: "bigserial", primaryKey: true },
    // Дедупликация пачек Telegram: повторная пачка = «уже сохранено», не ошибка
    telegram_update_id: { type: "bigint", notNull: true, unique: true },
    chat_id: { type: "bigint", notNull: true },
    chat_type: { type: "text", notNull: true }, // private | group
    actor_id: { type: "bigint" },
    kind: { type: "text", notNull: true }, // message | reply | mention | command | edited | other
    text: { type: "text" },
    raw_update: { type: "jsonb", notNull: true },
    // pending | journal_only | buffered | processed
    status: { type: "text", notNull: true, default: "pending" },
    turn_id: { type: "bigint" },
    occurred_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.createIndex("social_events", ["status", "chat_id"]);
  pgm.createIndex("social_events", ["turn_id"]);

  // Offset двигается только в той же транзакции, что и запись события (§6.1)
  pgm.createTable("poll_state", {
    key: { type: "text", primaryKey: true },
    value: { type: "bigint", notNull: true },
    updated_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });

  // Turn: постоянный ID batch; принадлежность событий переживёт потерю таймера (§6.2)
  pgm.createTable("turns", {
    id: { type: "bigserial", primaryKey: true },
    chat_id: { type: "bigint", notNull: true },
    // created | applied (applied = outbox создан, повторное применение ничего не меняет)
    status: { type: "text", notNull: true, default: "created" },
    created_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.createIndex("turns", ["chat_id", "created_at"]);

  pgm.createTable("delivery_outbox", {
    id: { type: "bigserial", primaryKey: true },
    turn_id: { type: "bigint", notNull: true },
    // Позиция пузыря в ответе; внутренний ключ против дублей намерений (§13.1)
    position: { type: "integer", notNull: true },
    chat_id: { type: "bigint", notNull: true },
    payload: { type: "jsonb", notNull: true },
    not_before: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
    // pending | sending | sent | retry_wait | unknown | cancelled | failed (§13.1)
    status: { type: "text", notNull: true, default: "pending" },
    attempts: { type: "integer", notNull: true, default: 0 },
    telegram_message_id: { type: "bigint" },
    last_error: { type: "text" },
    created_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
    updated_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.addConstraint("delivery_outbox", "delivery_outbox_turn_position_key", {
    unique: ["turn_id", "position"],
  });
  pgm.createIndex("delivery_outbox", ["status", "not_before"]);
  pgm.createIndex("delivery_outbox", ["chat_id", "turn_id", "position"]);
};

export const down = (pgm) => {
  pgm.dropTable("delivery_outbox");
  pgm.dropTable("turns");
  pgm.dropTable("poll_state");
  pgm.dropTable("social_events");
};
