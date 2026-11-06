// Абляции replay (40.2): меняют запрос из дампа, чтобы проверить гипотезу
// «модель подражает своим прошлым репликам». Схема TurnResult и сама модель
// не меняются — меняется только то, что подаётся на вход.
package modebug

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PanelMarkers — маркеры панели состояния панели хода (character экспортирует
// их как StatePanelMarkers): абляция no-panel должна резать ровно панель.
type PanelMarkers struct {
	Open  string
	Close string
}

// messageLabelRe — метка сообщения #m<id> вместе с предшествующими пробелами
// («[время] #m1034 Коля: …» → «[время] Коля: …»).
var messageLabelRe = regexp.MustCompile(`\s*#m\d+`)

// timestampRe — ведущая метка времени истории «[пт 06.11 12:01] ».
var timestampRe = regexp.MustCompile(`(?m)^\s*\[\s*(?:пн|вт|ср|чт|пт|сб|вс)\s+\d{2}\.\d{2}\s+\d{2}:\d{2}\s*\]\s*`)

// ApplyVariants применяет абляции к телу запроса (мутирует body — вызывающий
// делает копию на прогон). Неизвестная абляция — ошибка, а не тихое «ладно».
func ApplyVariants(body map[string]any, variants []string, panel PanelMarkers) error {
	for _, v := range variants {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if err := applyVariant(body, v, panel); err != nil {
			return err
		}
	}
	return nil
}

func applyVariant(body map[string]any, variant string, panel PanelMarkers) error {
	switch {
	case variant == "no-labels":
		transformMessages(body, func(s string) string {
			return messageLabelRe.ReplaceAllString(s, "")
		})
	case variant == "no-timestamps":
		transformMessages(body, func(s string) string {
			return timestampRe.ReplaceAllString(s, "")
		})
	case variant == "no-panel":
		return dropPanel(body, panel)
	case variant == "assistant-as-json":
		return assistantAsJSON(body)
	case variant == "merge-assistant":
		return mergeAssistant(body)
	case variant == "no-tools":
		delete(body, "tools")
		delete(body, "tool_choice")
	case strings.HasPrefix(variant, "history-last-"):
		n, err := strconv.Atoi(strings.TrimPrefix(variant, "history-last-"))
		if err != nil || n <= 0 {
			return fmt.Errorf("абляция %q: нужно положительное N (history-last-10)", variant)
		}
		return historyLastN(body, n)
	case strings.HasPrefix(variant, "temperature="), strings.HasPrefix(variant, "temperature:"):
		raw := strings.TrimPrefix(strings.TrimPrefix(variant, "temperature="), "temperature:")
		t, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return fmt.Errorf("абляция %q: температура должна быть числом", variant)
		}
		body["temperature"] = t
	default:
		return fmt.Errorf("неизвестная абляция %q: допустимы no-labels, no-timestamps, no-panel, history-last-N, assistant-as-json, merge-assistant, no-tools, temperature=X", variant)
	}
	return nil
}

// transformMessages применяет f к тексту всех сообщений (строки или текстовые
// части мультимодального content). Служебные поля (tool_calls) не трогаем.
func transformMessages(body map[string]any, f func(string) string) {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		mm["content"] = transformContent(mm["content"], f)
	}
}

// transformContent применяет f к строке content или к текстовым частям.
func transformContent(content any, f func(string) string) any {
	switch c := content.(type) {
	case string:
		return f(c)
	case []any:
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok || pm["type"] != "text" {
				continue
			}
			if text, ok := pm["text"].(string); ok {
				pm["text"] = f(text)
			}
		}
		return c
	default:
		return content
	}
}

// dropPanel вырезает панель состояния из последнего user-сообщения (в том
// числе из текстовой части мультимодального content).
func dropPanel(body map[string]any, panel PanelMarkers) error {
	if panel.Open == "" || panel.Close == "" {
		return nil
	}
	msgs, ok := body["messages"].([]any)
	if !ok {
		return nil
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		mm, ok := msgs[i].(map[string]any)
		if !ok || mm["role"] != "user" {
			continue
		}
		mm["content"] = stripPanelContent(mm["content"], panel)
		return nil
	}
	return nil
}

// stripPanelContent применяет stripPanel к строке или к текстовым частям.
func stripPanelContent(content any, panel PanelMarkers) any {
	switch c := content.(type) {
	case string:
		return stripPanel(c, panel)
	case []any:
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok || pm["type"] != "text" {
				continue
			}
			if text, ok := pm["text"].(string); ok {
				pm["text"] = stripPanel(text, panel)
			}
		}
		return c
	default:
		return content
	}
}

// stripPanel удаляет блок панели (вместе с маркерами) и лишние края.
func stripPanel(s string, panel PanelMarkers) string {
	start := strings.Index(s, panel.Open)
	if start < 0 {
		return s
	}
	tail := s[start+len(panel.Open):]
	endRel := strings.Index(tail, panel.Close)
	if endRel < 0 {
		// Панель без закрывающего маркера: режем до конца — иначе маркер
		// останется в запросе и абляция будет ложной.
		return strings.TrimSpace(s[:start])
	}
	end := start + len(panel.Open) + endRel + len(panel.Close)
	return strings.TrimSpace(s[:start] + s[end:])
}

// historyLastN оставляет system-сообщения и N последних прочих сообщений.
func historyLastN(body map[string]any, n int) error {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return nil
	}
	system := make([]any, 0, 1)
	rest := make([]any, 0, len(msgs))
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "system" {
			system = append(system, m)
			continue
		}
		rest = append(rest, m)
	}
	if len(rest) > n {
		rest = rest[len(rest)-n:]
	}
	out := append(system, rest...)
	body["messages"] = out
	return nil
}

// assistantText — assistant-сообщение с текстом и без tool_calls (историю
// разговора без инструментов можно переупаковывать безопасно).
func assistantText(m any) (map[string]any, string, bool) {
	mm, ok := m.(map[string]any)
	if !ok || mm["role"] != "assistant" {
		return nil, "", false
	}
	if _, has := mm["tool_calls"]; has {
		return nil, "", false
	}
	text, ok := mm["content"].(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil, "", false
	}
	return mm, text, true
}

// assistantAsJSON оборачивает подряд идущие assistant-реплики в TurnResult-JSON:
// модель видит, что прошлые ходы были JSON, а не плейн-текстом.
func assistantAsJSON(body map[string]any) error {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		_, _, isAssistant := assistantText(msgs[i])
		if !isAssistant {
			out = append(out, msgs[i])
			continue
		}
		texts := make([]string, 0, 2)
		j := i
		for ; j < len(msgs); j++ {
			_, text, ok := assistantText(msgs[j])
			if !ok {
				break
			}
			texts = append(texts, text)
		}
		wrapped, err := json.Marshal(map[string]any{"decision": "answer", "messages": texts})
		if err != nil {
			return fmt.Errorf("assistant-as-json: не сериализуется: %w", err)
		}
		out = append(out, map[string]any{"role": "assistant", "content": string(wrapped)})
		i = j - 1
	}
	body["messages"] = out
	return nil
}

// mergeAssistant склеивает подряд идущие assistant-реплики в одно сообщение.
func mergeAssistant(body map[string]any) error {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		_, _, isAssistant := assistantText(msgs[i])
		if !isAssistant {
			out = append(out, msgs[i])
			continue
		}
		texts := make([]string, 0, 2)
		j := i
		for ; j < len(msgs); j++ {
			_, text, ok := assistantText(msgs[j])
			if !ok {
				break
			}
			texts = append(texts, text)
		}
		out = append(out, map[string]any{"role": "assistant", "content": strings.Join(texts, "\n")})
		i = j - 1
	}
	body["messages"] = out
	return nil
}
