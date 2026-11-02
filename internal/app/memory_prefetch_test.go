// Юнит-тест флага предзагрузки памяти на уровне сборки приложения: без
// настроенной модели (Config.Model == nil, модель подставлена тестом) действует
// прод-умолчание MODEL_MEMORY_PREFETCH=true (решение 2026-11-02).
package app

import (
	"testing"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

func TestMemoryPrefetchDefault(t *testing.T) {
	if !memoryPrefetch(runtime.Config{}) {
		t.Fatalf("без Config.Model предзагрузка памяти должна быть включена")
	}
	if !memoryPrefetch(runtime.Config{Model: &runtime.ModelSettings{MemoryPrefetch: true}}) {
		t.Fatalf("явный true не применён")
	}
	if memoryPrefetch(runtime.Config{Model: &runtime.ModelSettings{MemoryPrefetch: false}}) {
		t.Fatalf("явный false не применён")
	}
}
