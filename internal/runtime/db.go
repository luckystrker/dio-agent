// Подключение к PostgreSQL (pgx/v5) — единственная внешняя зависимость
// приложения (философия №6). Пул используется всеми модулями; singleton-lock
// и лимиты соединений приходят с этапами 9.7/9.x.
package runtime

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool создаёт пул соединений по DSN из конфигурации.
// Соединение сразу проверяется Ping-ом: невалидный DSN или недоступная база —
// явная ошибка запуска, а не молчаливый пул.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL не разбирается: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("пул PostgreSQL не создан: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("PostgreSQL недоступен: %w", err)
	}
	return pool, nil
}
