//go:build db

// Сквозная цепочка 18.5 (мутация M12 приёмки): DB_MAX_CONNS проходит от
// конфига приложения до конфигурации пула. Юнит-тесты бьют по звеньям
// отдельно (env→Config.DBMaxConns в config_test.go, poolConfig→cfg.MaxConns
// в runtime/db_test.go) — мутация «NewPool без третьего аргумента» в app.go
// ими не убивалась: харнессы стартуют с DBMaxConns=0, и дефолт 8 совпадает с
// мутантным поведением. Здесь проверяется проводка целиком: заметное в
// конфиге значение (3 — не равно ни DefaultMaxConns=8, ни дефолту pgx
// max(4, NumCPU)) обязано оказаться в потолке живого пула приложения.
package app

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

func TestAppPoolMaxConnsFromConfig(t *testing.T) {
	dsn := newTestDB(t)
	a, err := New(Options{
		Config:         runtime.Config{DatabaseURL: dsn, NodeEnv: runtime.NodeEnvTest, DBMaxConns: 3},
		Logger:         log.New(io.Discard, "", 0),
		SingletonWatch: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("приложение не собралось: %v", err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("приложение не запустилось: %v", err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })

	if a.pool == nil {
		t.Fatalf("пул не собран")
	}
	if got := a.pool.Config().MaxConns; got != 3 {
		t.Fatalf("MaxConns пула приложения = %d, хочу 3 из Config.DBMaxConns — DB_MAX_CONNS не доезжает до пула (18.5)", got)
	}
}
