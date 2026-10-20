// Включает расширения PostgreSQL, нужные проекту.
// pgvector — семантический поиск по памяти (ARCHITECTURE_v0.6 §16, §18).

export const up = (pgm) => {
  pgm.sql("CREATE EXTENSION IF NOT EXISTS vector");
};

export const down = (pgm) => {
  pgm.sql("DROP EXTENSION IF EXISTS vector");
};
