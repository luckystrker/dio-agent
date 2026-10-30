-- Семантический поиск по памяти (спека §10.4 п.4, §18): эмбеддинги записей.
-- Отдельная таблица, а не колонка memory_items: модель и вектор меняются
-- вместе (переиндексация при смене модели), scope-фильтр остаётся в
-- реляционной части и применяется ДО ранжирования (§9.2).
-- Вектор без размерности: размер задаётся настроенной моделью эмбеддингов
-- (EMBEDDING_*), точный перебор вместо ANN-индекса — объёмы персонального
-- агента малы, §18 ANN-индекс не требует.
CREATE TABLE memory_embeddings (
    memory_id  bigint PRIMARY KEY REFERENCES memory_items(id) ON DELETE CASCADE,
    model      text   NOT NULL,
    embedding  vector NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
