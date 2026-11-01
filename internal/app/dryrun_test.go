// Юнит-тесты dry-run заглушки Bot API (16.6): заглушка пишет отправки в
// логгер, данный ей сборкой приложения, — живой локальный прогон по RUNBOOK
// без токена видит, что персонаж отправлял бы.
package app

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// TestDryRunAPILogsSends — заглушка пишет все вызовы отправки в свой логгер.
func TestDryRunAPILogsSends(t *testing.T) {
	var buf bytes.Buffer
	api := &dryRunAPI{logger: log.New(&buf, "", 0)}
	ctx := context.Background()

	if _, err := api.SendMessage(ctx, 42, "привет"); err != nil {
		t.Fatalf("sendMessage упал: %v", err)
	}
	if err := api.SetMessageReaction(ctx, 42, 7, "👀"); err != nil {
		t.Fatalf("setMessageReaction упал: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sendMessage chat=42: привет") {
		t.Fatalf("sendMessage не попал в лог: %q", out)
	}
	if !strings.Contains(out, "setMessageReaction chat=42 message=7: 👀") {
		t.Fatalf("setMessageReaction не попал в лог: %q", out)
	}
}

// TestBuildTelegramAPIDryRunNilLogger — ветка защиты app.go: nil-логгер не
// роняет сборку (подставляется дефолтный stderr-логгер), заглушка работает
// (log.Printf по nil-логгеру паниковал бы при первой отправке).
func TestBuildTelegramAPIDryRunNilLogger(t *testing.T) {
	cfg := runtime.Config{Telegram: &runtime.TelegramSettings{DryRun: true}}

	api := buildTelegramAPI(cfg, nil)
	stub, ok := api.(*dryRunAPI)
	if !ok {
		t.Fatalf("dry-run должен собрать dryRunAPI, собрано %T", api)
	}
	if stub.logger == nil {
		t.Fatal("логгер заглушки не подставлен: первая отправка запанирует")
	}
	if _, err := stub.SendMessage(context.Background(), 1, "без логгера"); err != nil {
		t.Fatalf("sendMessage упал: %v", err)
	}
}

// TestBuildTelegramAPIDryRunUsesAppLogger — сборка передаёт заглушке логгер
// приложения, а не io.Discard: dry-run отправки видны в журнале процесса.
func TestBuildTelegramAPIDryRunUsesAppLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "dio: ", 0)
	cfg := runtime.Config{Telegram: &runtime.TelegramSettings{DryRun: true}}

	api := buildTelegramAPI(cfg, logger)
	stub, ok := api.(*dryRunAPI)
	if !ok {
		t.Fatalf("dry-run должен собрать dryRunAPI, собрано %T", api)
	}
	if _, err := stub.SendMessage(context.Background(), 1, "проверка лога"); err != nil {
		t.Fatalf("sendMessage упал: %v", err)
	}
	if !strings.Contains(buf.String(), "проверка лога") {
		t.Fatalf("отправка dry-run не дошла до логгера приложения: %q", buf.String())
	}
}
