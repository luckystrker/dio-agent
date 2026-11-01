// Страж спеки для 16.4 (замечание приёмки): решение «классификация ошибок
// polling; ядовитый update не глушит приём» обязано жить в источнике истины —
// docs/ARCHITECTURE_v0.7.md (журнал «Изменения решений» + §6.1), иначе спека
// молча расходится с кодом: CHECKLIST.md обещает «поведенческие решения
// фиксируются в спеке в коммитах соответствующих пунктов». По образцу
// character/shipped_files_test.go — читает файл репозитория, без БД.
package telegram

import (
	"os"
	"strings"
	"testing"
)

// spec164Section возвращает тело раздела спеки от заголовка до следующего «## ».
func spec164Section(t *testing.T, spec, heading string) string {
	t.Helper()
	start := strings.Index(spec, heading)
	if start < 0 {
		t.Fatalf("в спеке нет раздела %q", heading)
	}
	start += len(heading)
	end := len(spec)
	if next := strings.Index(spec[start:], "\n## "); next >= 0 {
		end = start + next
	}
	return spec[start:end]
}

func TestSpec164PollingErrorPolicyDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/ARCHITECTURE_v0.7.md")
	if err != nil {
		t.Fatalf("спека не читается: %v", err)
	}
	spec := string(raw)

	// Журнал «Изменения решений» несёт запись этапа 16.4 с полной
	// классификацией: 401/404 фатальны, 409 с нарастанием, ошибки данных
	// 22xxx/23xxx и нормализации — скип с продвижением offset.
	journal := spec164Section(t, spec, "## Изменения решений")
	if !strings.Contains(journal, "этап 16.4") {
		t.Fatalf("в журнале «Изменения решений» нет записи этапа 16.4 — классификация ошибок polling не задокументирована")
	}
	for _, anchor := range []string{"401", "404", "409", "22xxx", "23xxx"} {
		if !strings.Contains(journal, anchor) {
			t.Fatalf("запись этапа 16.4 не упоминает %q", anchor)
		}
	}

	// §6.1 больше не заявляет безусловного «пачка прекращается при любой
	// ошибке БД»: выписана ветка ошибок данных (скип с продвижением offset)
	// рядом с транзиентной (повтор пачки до retry).
	s61 := spec164Section(t, spec, "## 6.1 Короткая транзакция ingest")
	for _, anchor := range []string{"22xxx", "скип", "до retry"} {
		if !strings.Contains(s61, anchor) {
			t.Fatalf("§6.1 не упоминает %q — фраза про прекращение пачки расходится с кодом 16.4", anchor)
		}
	}
}
