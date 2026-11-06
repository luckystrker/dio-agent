// Набор инструментов и выбор режима инструментов разговорного хода (этап 39.2).
//
// Стабильный префикс (кэш провайдера): набор объявленных инструментов зависит
// ТОЛЬКО от конфигурации — есть ли память и включён ли веб, — а не от ситуации
// хода (план памяти выкл, суточный веб исчерпан, карточка полна). «На этом ходе
// инструменты не нужны» выражается tool_choice=none: определения остаются в
// запросе, поэтому начало запроса не меняется между ходами.
package character

// conversationToolPlan — что объявлено на ходе и какой режим выбора задать.
type conversationToolPlan struct {
	// Memory — инструменты памяти объявлены (есть тулсет памяти).
	Memory bool
	// Web — веб-инструменты объявлены (веб включён в конфигурации), в том числе
	// при исчерпанном суточном лимите: вызов отклонит лимит с понятным текстом.
	Web bool
	// Choice — режим выбора инструментов: "" — обычный (модель решает сама или
	// forced tool_choice), "none" — инструменты на этом ходе не нужны.
	Choice string
}

// planConversationTools решает набор и режим инструментов хода. Набор стабилен
// между ходами; меняется только Choice.
func planConversationTools(hasMemory, memoryNeeded, webConfigured, webAvailable bool) conversationToolPlan {
	plan := conversationToolPlan{Memory: hasMemory, Web: webConfigured}
	if !memoryNeeded && !webAvailable {
		plan.Choice = "none"
	}
	return plan
}

// toolChoiceLabel — человекочитаемый режим инструментов для лога хода.
func toolChoiceLabel(choice string) string {
	if choice == "none" {
		return "none (на этом ходе не нужны)"
	}
	return "auto"
}

// craneFallbackResult — тело стоп-крана, когда финальный вызов с
// tool_choice=none не дал валидного TurnResult (39.1): на прямом ходе видимая
// реакция «🤔» вместо молчания, на фоновом — ignore.
func craneFallbackResult(direct bool) TurnResult {
	if direct {
		return TurnResult{
			Decision: DecisionReact,
			Reaction: "🤔",
			Reason:   "стоп-кран: инструменты закрыты, но финальный ответ модели не пришёл — прямой ход завершён видимой реакцией",
		}
	}
	return TurnResult{
		Decision: DecisionIgnore,
		Reason:   "стоп-кран: инструменты закрыты, но финальный ответ модели не пришёл — ход завершён без ответа",
	}
}
