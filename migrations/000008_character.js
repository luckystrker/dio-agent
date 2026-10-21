// Персонаж (этап 4): persona с версиями, mood, relationships,
// подавление инициативы, имя автора события.

export const up = (pgm) => {
  pgm.createTable("personas", {
    id: { type: "bigserial", primaryKey: true },
    version: { type: "integer", notNull: true, unique: true },
    active: { type: "boolean", notNull: true, default: true },
    name: { type: "text", notNull: true },
    description: { type: "text", notNull: true },
    speech_style: { type: "text", notNull: true },
    values: { type: "jsonb", notNull: true, default: "[]" },
    mood_valence: { type: "numeric", notNull: true, default: "0" },
    mood_arousal: { type: "numeric", notNull: true, default: "0.4" },
    mood_social_energy: { type: "numeric", notNull: true, default: "0.6" },
    // persona_seed — художественная биография, помечена как вымысел (§7.1)
    seed_bio: { type: "text" },
    created_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });

  // Singleton-строка id=1 (§7.2)
  pgm.createTable("mood_state", {
    id: { type: "integer", primaryKey: true },
    valence: { type: "numeric", notNull: true },
    arousal: { type: "numeric", notNull: true },
    social_energy: { type: "numeric", notNull: true },
    updated_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });

  pgm.createTable("relationships", {
    person_entity_id: {
      type: "bigint",
      notNull: true,
      references: { schema: "public", name: "entities" },
    },
    scope_id: { type: "text", notNull: true },
    familiarity: { type: "numeric", notNull: true, default: "0" },
    affinity: { type: "numeric", notNull: true, default: "0" },
    trust: { type: "numeric", notNull: true, default: "0.2" },
    updated_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.addConstraint("relationships", "relationships_pk", {
    primaryKey: ["person_entity_id", "scope_id"],
  });

  pgm.createTable("initiative_topics", {
    entity_id: { type: "bigint", notNull: true, references: { schema: "public", name: "entities" } },
    scope_id: { type: "text", notNull: true },
    last_initiative_at: { type: "timestamptz", notNull: true, default: pgm.func("now()") },
  });
  pgm.addConstraint("initiative_topics", "initiative_topics_pk", {
    primaryKey: ["entity_id", "scope_id"],
  });

  pgm.addColumn("social_events", {
    actor_name: { type: "text" },
  });
};

export const down = (pgm) => {
  pgm.dropColumn("social_events", "actor_name");
  pgm.dropTable("initiative_topics");
  pgm.dropTable("relationships");
  pgm.dropTable("mood_state");
  pgm.dropTable("personas");
};
