package runtime

import (
	"reflect"
	"strings"
	"testing"
)

// Whitelist лички (TELEGRAM_PRIVATE_ALLOWLIST): список user id через запятую;
// пусто — личка открыта всем.
func TestLoadConfigPrivateAllowlist(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "x", "TELEGRAM_BOT_TOKEN": "t"}

	cfg, err := LoadConfig(envOf(base))
	if err != nil {
		t.Fatalf("ошибка: %v", err)
	}
	if cfg.Telegram.PrivateAllowlist != nil {
		t.Fatalf("без переменной whitelist должен быть пуст, получено %v", cfg.Telegram.PrivateAllowlist)
	}

	with := map[string]string{"DATABASE_URL": "x", "TELEGRAM_BOT_TOKEN": "t",
		"TELEGRAM_PRIVATE_ALLOWLIST": " 722958928, 42 ,"}
	cfg, err = LoadConfig(envOf(with))
	if err != nil {
		t.Fatalf("ошибка: %v", err)
	}
	if want := []int64{722958928, 42}; !reflect.DeepEqual(cfg.Telegram.PrivateAllowlist, want) {
		t.Fatalf("whitelist = %v, хочу %v", cfg.Telegram.PrivateAllowlist, want)
	}

	bad := map[string]string{"DATABASE_URL": "x", "TELEGRAM_BOT_TOKEN": "t",
		"TELEGRAM_PRIVATE_ALLOWLIST": "722958928,abc"}
	if _, err := LoadConfig(envOf(bad)); err == nil || !strings.Contains(err.Error(), "TELEGRAM_PRIVATE_ALLOWLIST") {
		t.Fatalf("мусор в whitelist должен быть ошибкой с именем переменной, получено %v", err)
	}

	// m2 ревью 2026-11-07: id лички — положительный user id; 0 (синтетический
	// чат неподдерживаемых updates) и отрицательные (группы) в списке
	// бессмысленны — fail-fast вместо ложного ощущения настройки.
	for _, id := range []string{"0", "-5"} {
		cfg := map[string]string{"DATABASE_URL": "x", "TELEGRAM_BOT_TOKEN": "t",
			"TELEGRAM_PRIVATE_ALLOWLIST": "722958928," + id}
		if _, err := LoadConfig(envOf(cfg)); err == nil || !strings.Contains(err.Error(), "TELEGRAM_PRIVATE_ALLOWLIST") {
			t.Fatalf("id %q в whitelist должен быть ошибкой с именем переменной, получено %v", id, err)
		}
	}
}
