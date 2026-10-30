// Scope-политика памяти (спека §9.1–§9.2): public | chat:<id> | internal.
//
// Группа ≠ public; DM-факт не переносится в группу даже при том же
// пользователе. Пересечение разрешений производного объекта — пересечение
// скоупов его источников; пусто → internal. Missing/invalid provenance
// → internal. Фильтр применяется ДО выдачи результата модели: до извлечения
// текста и до семантического ранжирования.
package memory

import "regexp"

// Разрешённые скоупы (§9.1).
const (
	ScopePublic   = "public"
	ScopeInternal = "internal"
)

// chatScopePattern — chat:<целое число, возможно отрицательное> (Telegram ID).
var chatScopePattern = regexp.MustCompile(`^chat:-?\d+$`)

// ChatScope строит скоуп чата из Telegram chat id.
func ChatScope(chatID string) string { return "chat:" + chatID }

// IsValidScope проверяет разрешённый формат скоупа (§9.1).
func IsValidScope(scope string) bool {
	return scope == ScopePublic || scope == ScopeInternal || chatScopePattern.MatchString(scope)
}

// IntersectScopes — пересечение аудиторий (§9.1): производный объект доступен
// там, где доступны ВСЕ его источники. У двух разных приватных аудиторий
// общего чата может не быть — тогда internal. internal никогда не расширяет
// доступ; public не сужает. Пустой список — internal (нет источников, нет
// разрешения — то же, что missing provenance).
func IntersectScopes(scopes []string) string {
	unique := make([]string, 0, len(scopes))
	seen := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		if !seen[s] {
			seen[s] = true
			unique = append(unique, s)
		}
	}
	switch len(unique) {
	case 0:
		return ScopeInternal
	case 1:
		return unique[0]
	}
	for _, s := range unique {
		if s == ScopeInternal {
			return ScopeInternal
		}
	}
	meaningful := make([]string, 0, len(unique))
	for _, s := range unique {
		if s != ScopePublic {
			meaningful = append(meaningful, s)
		}
	}
	switch len(meaningful) {
	case 0:
		return ScopePublic
	case 1:
		return meaningful[0]
	default:
		return ScopeInternal
	}
}

// VisibleFrom — виден ли объект с назначенным скоупом из аудитории.
// internal виден только внутреннему контексту runtime (рефлексия работает
// отдельно, её производные наследуют ограничения — §9.2).
func VisibleFrom(objectScope, audience string) bool {
	return objectScope == ScopePublic || objectScope == audience
}

// ItemVisible — финальный предикат выдачи записи памяти аудитории (§9.2).
// Одно из двух ограничений одно: назначенный runtime скоуп записи и
// разрешённые аудитории provenance — видимость требует ОБА, расширить доступ
// друг через друга невозможно («расширение доступа требует явной
// валидированной операции», §9.1).
//
// Missing/invalid provenance → internal (§9.1): запись с отсутствующим или
// битым разрешением ведёт себя как internal — видна только внутреннему
// контексту. Элемент provenance с невалидным форматом портит всё разрешение
// целиком: частично доверять списку нельзя.
//
// Аудитория internal видит всё: рефлексия — внутренняя работа runtime, её
// записи наследуют ограничения при записи производных (§9.2), а не при
// чтении источников.
func ItemVisible(scopeID string, provenanceExists bool, provenanceScopes []string, audience string) bool {
	if audience == ScopeInternal {
		return true
	}
	if !provenanceExists || len(provenanceScopes) == 0 {
		return false // missing/invalid → internal, чатам не выдаётся
	}
	for _, s := range provenanceScopes {
		if !IsValidScope(s) {
			return false // invalid provenance → internal (§9.1)
		}
	}
	if !VisibleFrom(scopeID, audience) {
		return false
	}
	for _, s := range provenanceScopes {
		if s == audience || s == ScopePublic {
			return true
		}
	}
	return false
}
