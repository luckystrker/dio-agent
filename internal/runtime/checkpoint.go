// Адаптер чекпоинтов: task.CheckpointStore фреймворка ago поверх таблицы
// task_checkpoints (§12.2, §18). Состояние чекпоинта выбирает владелец задачи —
// фреймворк содержимое не читает; соглашение адаптера: состояние — JSON-документ
// (колонка state имеет тип jsonb). JSONB канонизирует формат (пробелы, порядок
// ключей), но сохраняет значение — Read возвращает то же JSON-значение, что
// было записано.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agotask "ago/task"
)

// ErrInvalidCheckpoint — Write отклоняет состояние, которое не является
// корректным JSON: молчаливая порча чекпоинта недопустима (чекпоинт —
// точка восстановления долгой задачи, §12.2).
var ErrInvalidCheckpoint = errors.New("чекпоинт не является корректным JSON")

// PgCheckpointStore — реализация agotask.CheckpointStore на PostgreSQL.
// Контекст — как у PgStreamStorage: задаётся при создании.
type PgCheckpointStore struct {
	ctx  context.Context
	pool *pgxpool.Pool
}

// Интерфейс ago должен выполняться во время компиляции.
var _ agotask.CheckpointStore = (*PgCheckpointStore)(nil)

// NewPgCheckpointStore создаёт адаптер чекпоинтов поверх пула PostgreSQL.
func NewPgCheckpointStore(ctx context.Context, pool *pgxpool.Pool) *PgCheckpointStore {
	return &PgCheckpointStore{ctx: ctx, pool: pool}
}

// Write сохраняет состояние задачи, заменяя прежний чекпоинт (паритет
// MemoryCheckpointStore: «replacing any previous checkpoint»).
func (c *PgCheckpointStore) Write(taskID string, state []byte) error {
	if taskID == "" {
		return errors.New("чекпоинт: пустой task_id")
	}
	if len(state) == 0 || !json.Valid(state) {
		return fmt.Errorf("%w: задача %q", ErrInvalidCheckpoint, taskID)
	}
	_, err := c.pool.Exec(c.ctx, `
		INSERT INTO task_checkpoints (task_id, state)
		VALUES ($1, $2::jsonb)
		ON CONFLICT (task_id) DO UPDATE
		SET state = EXCLUDED.state, updated_at = now()`,
		taskID, string(state))
	if err != nil {
		return fmt.Errorf("чекпоинт: задача %q не записана: %w", taskID, err)
	}
	return nil
}

// Read возвращает состояние задачи; отсутствие чекпоинта — (nil, nil)
// по контракту интерфейса (ago/task/checkpoint.go:8).
func (c *PgCheckpointStore) Read(taskID string) ([]byte, error) {
	var state []byte
	err := c.pool.QueryRow(c.ctx,
		`SELECT state FROM task_checkpoints WHERE task_id = $1`, taskID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("чекпоинт: задача %q не читается: %w", taskID, err)
	}
	return state, nil
}
