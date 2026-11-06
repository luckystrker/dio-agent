// Команда replay (этап 40.2): проиграть сохранённый дамп модельного вызова,
// применив абляции. Ничего не пишет в БД и не отправляет в Telegram — только
// HTTP к тому же провайдеру с ключом из .env/окружения (конфиг как у бота).
//
// Цель — проверять гипотезы о «каше» в ответах на живом запросе: убрать метки
// сообщений, метки времени, панель, упаковать прошлые реплики в JSON и т.п.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/modebug"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

// replayUsage — справка подкоманды.
const replayUsage = `dio-agent replay <дамп.json> [флаги]

Проигрывает сохранённый дамп модельного вызова (MODEL_DEBUG_DUMP_DIR, 40.1)
к тому же провайдеру с ключом из .env. В базу и Telegram ничего не пишет.

Флаги:
  --variant v1,v2   абляции (можно повторять):
                    no-labels        вырезать метки #mNNN из всех messages
                    no-timestamps    срезать «[пн 02.11 17:02] » из messages
                    no-panel         убрать панель из последнего user
                    history-last-N   оставить system + N последних сообщений
                    assistant-as-json  реплики бота — JSON TurnResult
                    merge-assistant  склеить подряд идущие реплики бота
                    no-tools         убрать tools/tool_choice
                    temperature=X    подменить температуру
  --n K             повторить запрос K раз (стохастика), показать долю брака

Примеры:
  dio-agent replay /var/lib/dio/dumps/turn-246-call-1.json
  dio-agent replay dump.json --variant no-labels --n 5
  dio-agent replay dump.json --variant no-panel,assistant-as-json
`

// variantList — флаг --variant с повторами и запятыми.
type variantList []string

func (v *variantList) String() string { return strings.Join(*v, ",") }

func (v *variantList) Set(raw string) error {
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*v = append(*v, part)
		}
	}
	return nil
}

// replay — точка входа подкоманды.
func replay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var variants variantList
	var runs int
	fs.Var(&variants, "variant", "абляции через запятую (можно повторять)")
	fs.IntVar(&runs, "n", 1, "сколько раз повторить запрос")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%v\n\n%s", err, replayUsage)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("нужен ровно один дамп: replay <файл>\n\n%s", replayUsage)
	}
	if runs < 1 {
		return fmt.Errorf("--n должно быть положительным числом")
	}

	if err := runtime.LoadDotEnv(".env"); err != nil {
		return err
	}
	settings, err := runtime.LoadModelSettings(runtime.OSEnv)
	if err != nil {
		return err
	}
	if settings == nil {
		return fmt.Errorf("модель не настроена: MODEL_ENDPOINT/MODEL_API_KEY/MODEL_MODEL нужны в .env или окружении")
	}

	dump, err := modebug.LoadDump(fs.Arg(0))
	if err != nil {
		return err
	}
	var request map[string]any
	if err := json.Unmarshal(dump.Request, &request); err != nil {
		return fmt.Errorf("request дампа не JSON-объект: %w", err)
	}

	open, closeMark := character.StatePanelMarkers()
	panel := modebug.PanelMarkers{Open: open, Close: closeMark}
	rep := &modebug.Replayer{
		Endpoint: settings.Endpoint,
		APIKey:   settings.APIKey,
		Timeout:  time.Duration(settings.TimeoutMs) * time.Millisecond,
	}

	subject := "turn-" + fmt.Sprint(dump.TurnID)
	if dump.TurnID == 0 && dump.Task != "" {
		subject = "bg-" + dump.Task
	}
	variantLabel := "без абляций"
	if len(variants) > 0 {
		variantLabel = strings.Join(variants, ", ")
	}
	fmt.Printf("дамп %s, вызов %d, модель %v\n", subject, dump.Call, request["model"])
	fmt.Printf("варианты: %s; прогонов: %d\n", variantLabel, runs)

	broken := 0
	for i := 1; i <= runs; i++ {
		body, err := cloneRequest(dump.Request)
		if err != nil {
			return err
		}
		warnings, err := modebug.ApplyVariants(body, variants, panel)
		if err != nil {
			return err
		}
		for _, warning := range warnings {
			fmt.Printf("предупреждение: %s\n", warning)
		}
		reply, err := rep.Send(context.Background(), dump.URL, body, dump.Headers)
		if err != nil {
			fmt.Printf("\n--- прогон %d/%d: ОШИБКА: %v\n", i, runs, err)
			broken++
			continue
		}
		parsed := modebug.ParseChatResponse(reply.Body)
		fmt.Printf("\n--- прогон %d/%d: %d, %d мс, finish_reason=%q\n",
			i, runs, reply.Status, reply.Latency.Milliseconds(), parsed.FinishReason)
		fmt.Printf("content:\n%s\n", printable(parsed.Content))
		if verdict, bad := replayVerdict(parsed.Content); bad {
			broken++
			fmt.Printf("TurnResult: брак — %s\n", verdict)
		} else {
			fmt.Printf("TurnResult: %s\n", verdict)
		}
		if parsed.Reasoning != "" {
			fmt.Printf("reasoning:\n%s\n", printable(parsed.Reasoning))
		} else {
			fmt.Printf("reasoning: пусто\n")
		}
	}
	if runs > 1 {
		fmt.Printf("\nитог: брак %d из %d (%d%%)\n", broken, runs, broken*100/runs)
	}
	return nil
}

// cloneRequest — глубокая копия тела запроса: каждая абляция и каждый прогон
// должны начинать с одного и того же запроса.
func cloneRequest(raw json.RawMessage) (map[string]any, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("request дампа не JSON-объект: %w", err)
	}
	return body, nil
}

// replayVerdict разбирает content как TurnResult (40.2, правила брака 40.4):
// валиден или брак. Пустой content — тоже брак: модель ничего не сказала.
func replayVerdict(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "пустой content", true
	}
	result, err := character.ParseTurnResult([]byte(trimmed))
	if err != nil {
		return "невалидный TurnResult: " + err.Error(), true
	}
	var bad []string
	for i, m := range result.Messages {
		if reason := character.MessageGarbageReason(m); reason != "" {
			bad = append(bad, fmt.Sprintf("messages[%d]: %s", i+1, reason))
		}
	}
	if len(bad) > 0 {
		return "брак в messages: " + strings.Join(bad, "; "), true
	}
	return fmt.Sprintf("валиден (decision=%s, сообщений %d)", result.Decision, len(result.Messages)), false
}

// printable — content для печати: как есть, но без гигантских простыней.
func printable(s string) string {
	const max = 8000
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
