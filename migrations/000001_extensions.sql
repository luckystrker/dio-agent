-- Расширения PostgreSQL (спека §18).
-- pgvector — семантический поиск по памяти (§10.4): embeddings добавляются
-- после базового retrieval, но extension включаем сразу, чтобы схема не менялась.

CREATE EXTENSION IF NOT EXISTS vector;
