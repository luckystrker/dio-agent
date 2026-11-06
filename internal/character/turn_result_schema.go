// JSON-схема TurnResult для провода (structured output провайдера). Строится
// программно из тех же констант лимитов, что проверяет валидатор, — расхождение
// схемы и валидатора исключено механикой (в TS согласованность проверял тест).
// На проводе схема превращается в strict json_schema response_format
// (config.RunChatCompat), а финальную валидацию правил §11.2 делает ParseTurnResult:
// discriminated-правила (answer требует сообщения, react — реакцию и т.п.) в
// JSON Schema выразимы плохо, а «ответ обязан валидироваться до применения» —
// требование §11.2/§17.
package character

// TurnSchemaFeatures — какие необязательные поля применимы на этом ходе
// (этап 38.1). Схема — часть запроса к модели, поэтому набор вариантов держим
// маленьким и детерминированным: поля всегда в одном порядке (map маршалится
// с сортировкой ключей), меняются только сами наборы.
type TurnSchemaFeatures struct {
	// Images — в пачке есть картинки (фото/стикер/видео): только тогда модель
	// может их описать. Нет картинок — поля image_notes в схеме нет.
	Images bool
	// UnknownSticker — в пачке есть стикер, которого нет в коллекции: только
	// тогда уместно save_sticker.
	UnknownSticker bool
	// StickerAllowed — коллекция не пуста и предохранитель частоты позволяет
	// отправить стикер на этом ходе.
	StickerAllowed bool
	// Web — веб-инструменты выданы на этом ходе: image_url принимается только
	// из их выдачи.
	Web bool
	// HistoryRefs — в истории/пачке есть метки сообщений #m<id>: только тогда
	// осмысленны reply/forward.
	HistoryRefs bool
}

// allTurnSchemaFeatures — полный набор (совместимость прежних вызовов).
func allTurnSchemaFeatures() TurnSchemaFeatures {
	return TurnSchemaFeatures{Images: true, UnknownSticker: true, StickerAllowed: true, Web: true, HistoryRefs: true}
}

// TurnResultJSONSchema — полная hand-written форма схемы (как в TS
// TURN_RESULT_JSON_SCHEMA), но лимиты подставлены из констант. Описания полей —
// подсказки модели. Используется там, где ситуация хода неизвестна (тесты,
// документирование контракта); на проводе — TurnResultJSONSchemaFor.
func TurnResultJSONSchema() map[string]any {
	return TurnResultJSONSchemaFor(allTurnSchemaFeatures())
}

// TurnResultJSONSchemaFor — схема хода по ситуации (38.1): неприменимые
// необязательные поля не попадают в схему вовсе, и модель не заполняет их
// мусором. Парсер при этом терпит лишние поля: необязательное поле, которого
// нет в схеме, мягко обнуляется, а не роняет ход.
func TurnResultJSONSchemaFor(f TurnSchemaFeatures) map[string]any {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"decision": map[string]any{"type": "string", "enum": []string{DecisionAnswer, DecisionReact, DecisionIgnore, DecisionDefer}},
			"messages": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string", "minLength": 1, "maxLength": maxMessageLength},
				"maxItems":    maxMessages,
				"description": "decision=answer: 1–4 сообщения; пустой массив допустим только если заполнен sticker или reaction.",
			},
			"notes": map[string]any{
				"type":     "array",
				"items":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxNoteLength},
				"maxItems": maxNotes,
			},
			"reply_to_by_message": map[string]any{
				"type":     "array",
				"maxItems": maxMessageReplies,
				"description": "Необязательно: ответить реплаем на конкретное сообщение. index — номер твоего сообщения в messages (с 1), ref — метка вида #m123 " +
					"из истории или пачки. Реплай возможен только на сообщение этого чата; неизвестная метка — сообщение уйдёт без реплая.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"index": map[string]any{"type": "integer", "minimum": 1, "maximum": maxMessages},
						"ref":   map[string]any{"type": "string", "minLength": 1, "maxLength": maxMessageLabelLen},
					},
					"required":             []string{"index", "ref"},
					"additionalProperties": false,
				},
			},
			"image_notes": map[string]any{
				"type":     "array",
				"maxItems": maxImageNotes,
				"description": "Описания картинок входящего сообщения по порядку нумерации («Картинки в сообщении: 1 — …; 2–6 — …»): одна короткая фраза " +
					"на номер. Кадры одного видео — одно описание на всё видео. Коротко, что видно, без оценок. Нет картинок — не заполняй поле.",
				"items": map[string]any{"type": "string", "maxLength": maxImageNoteLength},
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
				"description": "Стикер из твоей коллекции в панели состояния: номер из списка коллекции (#N) или эмодзи из списка. Стикер — редкая приправа, не в каждом ответе: " +
					"обычно либо стикер, либо реакция, не вместе и не каждый ход. Выбирай только из списка панели; не выдумывай метки и file_id. " +
					"Нет повода — не заполняй поле.",
			},
			"save_sticker": map[string]any{
				"type": "object",
				"description": "Сохранить в свою коллекцию стикер, присланный в этой пачке. Только если во входящей пачке есть стикер, которого НЕТ в твоей коллекции, " +
					"и он тебе нравится: добавь заметку, когда он уместен. В ходе без такого стикера поле не заполняй — runtime его отбросит.",
				"properties": map[string]any{
					"note": map[string]any{"type": "string", "minLength": 1, "maxLength": stickerNoteLimit},
				},
				"required":             []string{"note"},
				"additionalProperties": false,
			},
			"image_url": map[string]any{
				"type": "string", "minLength": 1, "maxLength": maxImageURLLen,
				"description": "Необязательная картинка к ответу (одна за ход). Только полный http(s)-адрес, который ты видел в результатах поиска или web_fetch ЭТОГО хода " +
					"(в том числе в списке «Картинки на странице»). Не придумывай адреса и не вставляй URL в сообщения: картинка уйдёт отдельным сообщением после текста.",
			},
			"forward": map[string]any{
				"type": "string", "minLength": 1, "maxLength": maxMessageLabelLen,
				"description": "Необязательно: переслать в этот же чат старое сообщение по метке (#m123). Только сообщение этого чата, одна пересылка за ход. " +
					"Пересылай редко — когда к месту напомнить старое сообщение.",
			},
			"intentions": map[string]any{
				"type":     "array",
				"maxItems": maxIntentions,
				"description": "До 2 отложенных действий: вернуться к теме (reply_later), спросить как дела (check_in), поделиться находкой (share), изучить тему (explore). " +
					"Время — относительное, в часах от сейчас.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind":   map[string]any{"type": "string", "enum": []string{"reply_later", "check_in", "share", "explore"}},
						"topic":  map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
						"reason": map[string]any{"type": "string", "minLength": 1, "maxLength": maxReasonLength},
						"delay_hours": map[string]any{"type": "number", "minimum": minDelayHours, "maximum": maxDeferHours,
							"description": DelayHoursHint},
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

	// Неприменимые поля убираем, а не оставляем пустыми: модель заполняет всё,
	// что видит в схеме (живой прогон 2026-11-04…06 — «forward», «sticker» и
	// выдуманные image_url почти в каждом ходе).
	props := schema["properties"].(map[string]any)
	if !f.Web {
		delete(props, "image_url")
	}
	if !f.Images {
		delete(props, "image_notes")
	}
	if !f.UnknownSticker {
		delete(props, "save_sticker")
	}
	if !f.StickerAllowed {
		delete(props, "sticker")
	}
	if !f.HistoryRefs {
		delete(props, "forward")
		delete(props, "reply_to_by_message")
	}
	return schema
}
