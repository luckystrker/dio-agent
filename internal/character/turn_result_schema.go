// JSON-схема TurnResult для провода (structured output провайдера). Строится
// программно из тех же констант лимитов, что проверяет валидатор, — расхождение
// схемы и валидатора исключено механикой (в TS согласованность проверял тест).
// На проводе схема превращается в strict json_schema response_format
// (config.RunChatCompat), а финальную валидацию правил §11.2 делает ParseTurnResult:
// discriminated-правила (answer требует сообщения, react — реакцию и т.п.) в
// JSON Schema выразимы плохо, а «ответ обязан валидироваться до применения» —
// требование §11.2/§17.
package character

// TurnResultJSONSchema — hand-written форма схемы (как в TS TURN_RESULT_JSON_SCHEMA),
// но лимиты подставлены из констант. Описания полей — подсказки модели.
func TurnResultJSONSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"decision": map[string]any{"type": "string", "enum": []string{DecisionAnswer, DecisionReact, DecisionIgnore, DecisionDefer}},
			"messages": map[string]any{
				"type":     "array",
				"items":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxMessageLength},
				"maxItems": maxMessages,
			},
			"notes": map[string]any{
				"type":     "array",
				"items":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxNoteLength},
				"maxItems": maxNotes,
			},
			"memory": map[string]any{
				"type":     "array",
				"maxItems": maxMemories,
				"description": "До 5 кандидатов долговременной памяти об ЭТОМ разговоре: факты о пользователе (claim), события (experience), твои мысли (thought), вкусы (preference). " +
					"Источники не указывай — их назначает система. Пиши только значимое, не каждое сообщение.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind": map[string]any{"type": "string", "enum": []string{"claim", "experience", "thought", "preference"}},
						"text": map[string]any{"type": "string", "minLength": 1, "maxLength": maxMemoryText},
						"entities": map[string]any{
							"type":     "array",
							"maxItems": maxEntityLinks,
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"name": map[string]any{"type": "string", "minLength": 1, "maxLength": maxEntityName},
									"kind": map[string]any{"type": "string", "enum": []string{"person", "topic", "place", "object", "event", "project"}},
									"role": map[string]any{"type": "string", "enum": []string{"subject", "participant", "place", "context"}},
								},
								"required":             []string{"name"},
								"additionalProperties": false,
							},
						},
						"salience":   map[string]any{"type": "number", "minimum": 0, "maximum": 1},
						"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1, "description": "Только для kind=claim"},
					},
					"required":             []string{"kind", "text"},
					"additionalProperties": false,
				},
			},
			"reaction": map[string]any{
				"type": "string", "minLength": 1, "maxLength": maxReactionLen,
				"enum": reactionEnum(),
				"description": "Эмодзи-реакция на последнее сообщение: при decision=react — сама реакция, при decision=answer — необязательное дополнение к тексту. " +
					"Только эмодзи из списка ReactionTypeEmoji Telegram — не текст и не произвольный символ.",
			},
			"sticker": map[string]any{
				"type": "string", "minLength": 1, "maxLength": maxStickerLabel,
				"description": "Метка стикера из твоей коллекции в панели состояния (например «#3»), если вместо слов честнее стикер. " +
					"Выбирай только из списка панели; не выдумывай метки и file_id. Необязательное дополнение к answer/react.",
			},
			"save_sticker": map[string]any{
				"type": "object",
				"description": "Сохранить в свою коллекцию стикер, присланный в этой пачке: добавь заметку, когда он уместен. " +
					"Только если стикера ещё нет в твоей коллекции и он тебе нравится.",
				"properties": map[string]any{
					"note": map[string]any{"type": "string", "minLength": 1, "maxLength": stickerNoteLimit},
				},
				"required":             []string{"note"},
				"additionalProperties": false,
			},
			"intentions": map[string]any{
				"type":     "array",
				"maxItems": maxIntentions,
				"description": "До 2 отложенных действий: вернуться к теме (reply_later), спросить как дела (check_in), поделиться находкой (share), изучить тему (explore). " +
					"Время — относительное, в часах от сейчас.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind":        map[string]any{"type": "string", "enum": []string{"reply_later", "check_in", "share", "explore"}},
						"topic":       map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
						"reason":      map[string]any{"type": "string", "minLength": 1, "maxLength": maxReasonLength},
						"delay_hours": map[string]any{"type": "number", "minimum": minDelayHours, "maximum": maxDeferHours},
					},
					"required":             []string{"kind", "topic", "reason", "delay_hours"},
					"additionalProperties": false,
				},
			},
			"defer_hours": map[string]any{
				"type": "number", "minimum": minDeferHours, "maximum": maxDeferHours,
				"description": "При decision=defer: через сколько часов вернуться к ответу",
			},
			"mood_delta": map[string]any{
				"type":        "object",
				"description": "Необязательное изменение твоего настроения этим ходом (маленькими шагами, с причиной)",
				"properties": map[string]any{
					"valence":      map[string]any{"type": "number", "minimum": -1, "maximum": 1},
					"arousal":      map[string]any{"type": "number", "minimum": 0, "maximum": 1},
					"socialEnergy": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
					"reason":       map[string]any{"type": "string", "minLength": 1, "maxLength": maxReasonLength},
				},
				"required":             []string{"reason"},
				"additionalProperties": false,
			},
			"relationship_delta": map[string]any{
				"type":        "object",
				"description": "Необязательное малое изменение отношения к собеседнику, с причиной",
				"properties": map[string]any{
					"affinity": map[string]any{"type": "number", "minimum": -1, "maximum": 1},
					"trust":    map[string]any{"type": "number", "minimum": 0, "maximum": 1},
					"reason":   map[string]any{"type": "string", "minLength": 1, "maxLength": maxReasonLength},
				},
				"required":             []string{"reason"},
				"additionalProperties": false,
			},
			"reason": map[string]any{"type": "string", "maxLength": maxReasonLength},
		},
		"required":             []string{"decision"},
		"additionalProperties": false,
	}
}
