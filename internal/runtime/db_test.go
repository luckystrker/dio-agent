// Юнит-тесты конфигурации пула соединений (18.5): DB_MAX_CONNS парсится
// конфигом (отдельный тест — config_test.go), а потолок реально доезжает до
// конфигурации пула pgx. Разбор DSN чист — живая база не нужна.
package runtime

import "testing"

func TestPoolConfigMaxConns(t *testing.T) {
	const dsn = "postgres://dio:dio@localhost:5433/dio?sslmode=disable"

	// Не задан — дефолт 8 (не max(4, NumCPU) pgx: рост с ядрами машины
	// незаметно упирается в лимит соединений сервера).
	cfg, err := poolConfig(dsn)
	if err != nil {
		t.Fatalf("DSN не разобран: %v", err)
	}
	if cfg.MaxConns != int32(DefaultMaxConns) {
		t.Fatalf("MaxConns = %d, хочу дефолт %d", cfg.MaxConns, DefaultMaxConns)
	}

	// Явное значение доезжает до конфигурации пула.
	cfg, err = poolConfig(dsn, 12)
	if err != nil {
		t.Fatalf("DSN не разобран: %v", err)
	}
	if cfg.MaxConns != 12 {
		t.Fatalf("MaxConns = %d, хочу 12", cfg.MaxConns)
	}

	// Нулевое/отрицательное (незаполненная группа конфига) — дефолт, не паника.
	cfg, err = poolConfig(dsn, 0)
	if err != nil {
		t.Fatalf("DSN не разобран: %v", err)
	}
	if cfg.MaxConns != int32(DefaultMaxConns) {
		t.Fatalf("MaxConns при 0 = %d, хочу дефолт %d", cfg.MaxConns, DefaultMaxConns)
	}
}
