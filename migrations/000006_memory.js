// Память (этап 3, §8–§10): entities, memory_items, links, sources,
// derivations, stances. Scope-изоляция: public | chat:<id> | internal (§9.1).

export const up = (pgm) => {
  pgm.createTable("entities", {
    id: { type: "bigserial", primaryKey: true },
    kind: { type: "text", notNull: true }, // person | topic | place | object | event | project
    canonical_name: { type: "text", notNull: true },
    scope_id: { type: "text", notNull: true, default: "internal" },
    created_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.createIndex("entities", ["scope_id", "canonical_name"]);

  pgm.createTable("entity_aliases", {
    id: { type: "bigserial", primaryKey: true },
    entity_id: { type: "bigint", notNull: true, references: { schema: "public", name: "entities" } },
    alias: { type: "text", notNull: true },
    scope_id: { type: "text", notNull: true },
    created_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.createIndex("entity_aliases", ["scope_id", "alias"]);

  pgm.createTable("sources", {
    id: { type: "bigserial", primaryKey: true },
    kind: { type: "text", notNull: true }, // journal_event | web_document | runtime_action | persona_version
    ref: { type: "text", notNull: true }, // стабильная ссылка: turn:42 / event:17 / url:...
    metadata: { type: "jsonb", notNull: true, default: "{}" },
    deletion_status: { type: "text", notNull: true, default: "active" }, // active | deleted
    created_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.createIndex("sources", ["kind", "ref"], { unique: true });

  pgm.createTable("memory_items", {
    id: { type: "bigserial", primaryKey: true },
    kind: { type: "text", notNull: true }, // claim | experience | thought | preference
    text: { type: "text", notNull: true },
    scope_id: { type: "text", notNull: true, default: "internal" },
    // active | disputed | superseded | retracted
    status: { type: "text", notNull: true, default: "active" },
    salience: { type: "numeric", notNull: true, default: "0.5" },
    confidence: { type: "numeric" }, // только для проверяемых claim
    occurred_at: { type: "timestamptz" },
    recorded_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
    valid_from: { type: "timestamptz" },
    valid_until: { type: "timestamptz" },
    supersedes_id: { type: "bigint" },
  });
  pgm.createIndex("memory_items", ["scope_id", "status", "recorded_at"]);
  pgm.createIndex("memory_items", "to_tsvector('russian', text)", { name: "memory_items_fts_idx" });

  pgm.createTable("memory_links", {
    memory_id: { type: "bigint", notNull: true, references: { schema: "public", name: "memory_items" } },
    entity_id: { type: "bigint", notNull: true, references: { schema: "public", name: "entities" } },
    role: { type: "text", notNull: true, default: "subject" }, // subject | participant | place | context
  });
  pgm.createIndex("memory_links", ["entity_id", "memory_id"]);
  pgm.createIndex("memory_links", ["memory_id"]);

  pgm.createTable("memory_sources", {
    memory_id: { type: "bigint", notNull: true, references: { schema: "public", name: "memory_items" } },
    source_id: { type: "bigint", notNull: true, references: { schema: "public", name: "sources" } },
  });
  pgm.createIndex("memory_sources", ["memory_id", "source_id"], { unique: true });

  pgm.createTable("memory_derivations", {
    derived_id: { type: "bigint", notNull: true, references: { schema: "public", name: "memory_items" } },
    input_memory_id: { type: "bigint", notNull: true, references: { schema: "public", name: "memory_items" } },
  });
  pgm.createIndex("memory_derivations", ["derived_id", "input_memory_id"], { unique: true });

  pgm.createTable("stances", {
    id: { type: "bigserial", primaryKey: true },
    entity_id: { type: "bigint", notNull: true, references: { schema: "public", name: "entities" } },
    aspect: { type: "text", notNull: true, default: "general" },
    scope_id: { type: "text", notNull: true },
    summary: { type: "text", notNull: true },
    strength: { type: "numeric", notNull: true, default: "0.3" }, // устойчивость позиции
    interest: { type: "numeric", notNull: true, default: "0.3" }, // желание возвращаться
    updated_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.createIndex("stances", ["entity_id", "aspect", "scope_id"], { unique: true });

  pgm.createTable("stance_basis", {
    stance_id: { type: "bigint", notNull: true, references: { schema: "public", name: "stances" } },
    memory_id: { type: "bigint", notNull: true, references: { schema: "public", name: "memory_items" } },
  });
  pgm.createIndex("stance_basis", ["stance_id", "memory_id"], { unique: true });

  // episode summaries: отметка о включении события в сводку (§10.2)
  pgm.addColumn("social_events", {
    summarized_at: { type: "timestamptz" },
  });
};

export const down = (pgm) => {
  pgm.dropColumn("social_events", "summarized_at");
  pgm.dropTable("stance_basis");
  pgm.dropTable("stances");
  pgm.dropTable("memory_derivations");
  pgm.dropTable("memory_sources");
  pgm.dropTable("memory_links");
  pgm.dropIndex("memory_items", "to_tsvector('russian', text)", { name: "memory_items_fts_idx" });
  pgm.dropTable("memory_items");
  pgm.dropTable("sources");
  pgm.dropTable("entity_aliases");
  pgm.dropTable("entities");
};
