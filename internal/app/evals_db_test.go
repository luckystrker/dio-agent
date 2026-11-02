//go:build db && eval

// Прогонщик поведенческих эвалов (§21.2, эталоны 8.2) поверх каркаса E2E:
// кейс = посев мира + пачка сообщений + ожидание терминального состояния хода,
// исход собирается в ago/eval.Trace и оценивается скорерами internal/evalsuite
// через eval.ScoreWith (ago/eval).
//
// Офлайн-режим (скриптовая модель, детерминированный гейт):
//
//	TEST_DATABASE_URL=… go test -tags "db,eval" ./internal/app/ -run TestEvalSuiteOffline
//
// Живой режим (реальная модель, стохастичные ответы — §21.2) — evals_live_test.go
// (тег live). Отчёт — evals/results/<режим>.json.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/eval"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/dio-agent/internal/telegram"

	"github.com/luckystrker/dio-agent/internal/evalsuite"
)

const evalSuitePath = "../../evals/behavior_suite.jsonl"

// evalRun — приложение одного кейса + собранный исход. wait — потолок
// ожидания терминального состояния хода: офлайн-модель отвечает мгновенно,
// живой нужно минуты.
type evalRun struct {
	h      *harness
	t      *testing.T
	chatID int64
	scope  string
	wait   time.Duration
	out    evalsuite.Outcome
}

// buildScripted превращает сценарий кейса в ответы скриптового дубля: JSON с
// ключом "tool" — ответ с вызовом инструмента (дубль умеет их, events.ToolCalls),
// остальное — тело финального TurnResult.
func buildScripted(t *testing.T, c evalsuite.Case) []*events.ModelResponse {
	t.Helper()
	var rs []*events.ModelResponse
	for i, body := range c.Scripted {
		var probe map[string]any
		if err := json.Unmarshal([]byte(body), &probe); err != nil {
			t.Fatalf("кейс %s: scripted[%d] не JSON: %v", c.Name, i, err)
		}
		if name, ok := probe["tool"].(string); ok {
			args, _ := json.Marshal(probe["args"])
			rs = append(rs, &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
				ID: fmt.Sprintf("call-eval-%d", i), Name: name, Arguments: string(args),
			}}}})
			continue
		}
		rs = append(rs, &events.ModelResponse{Message: &events.ModelMessage{Content: body}})
	}
	return rs
}

func newEvalHarness(t *testing.T, c evalsuite.Case) *evalRun {
	t.Helper()
	rs := buildScripted(t, c)
	tune := func(o *Options) {}
	if os.Getenv("EVAL_VERBOSE") != "" {
		tune = func(o *Options) { o.Logger = log.New(os.Stderr, "eval ", log.LstdFlags) }
	}
	h := newHarnessTuned(t, tune, rs...)
	scope := c.Setup.Scope
	if scope == "" {
		scope = "chat:100"
	}
	var chatID int64
	if _, err := fmt.Sscanf(scope, "chat:%d", &chatID); err != nil {
		t.Fatalf("скоуп кейса не chat:<id>: %q", scope)
	}
	return &evalRun{h: h, t: t, chatID: chatID, scope: scope}
}

// seed — посев мира: отношение к актору и память в скоупе кейса.
func (r *evalRun) seed(c evalsuite.Case) {
	ctx := context.Background()
	if rel := c.Setup.Relationship; rel != nil {
		if _, err := r.h.pool.Exec(ctx, `
			INSERT INTO entities (kind, canonical_name, scope_id) VALUES ('person', $1, $2)`,
			c.Setup.Actor, r.scope); err != nil {
			r.t.Fatalf("сущность актора не вставлена: %v", err)
		}
		if _, err := r.h.pool.Exec(ctx, `
			INSERT INTO relationships (person_entity_id, scope_id, familiarity, affinity, trust)
			SELECT id, $2, $3, $4, $5 FROM entities WHERE canonical_name = $1 AND scope_id = $2`,
			c.Setup.Actor, r.scope, rel.Familiarity, rel.Affinity, rel.Trust); err != nil {
			r.t.Fatalf("отношение не вставлено: %v", err)
		}
	}
	memScope := c.Setup.SeedScope
	if memScope == "" {
		memScope = r.scope
	}
	for i, m := range c.Setup.Memory {
		ref := fmt.Sprintf("event:eval_%s_%d", c.Name, i)
		if _, err := r.h.pool.Exec(ctx,
			`INSERT INTO provenance (kind, ref, scopes) VALUES ('journal_event', $1, ARRAY[$2])`,
			ref, memScope); err != nil {
			r.t.Fatalf("provenance не вставлена: %v", err)
		}
		if _, err := r.h.pool.Exec(ctx, `
			INSERT INTO memory_items (kind, text, scope_id, provenance_id, salience)
			SELECT $1, $2, $3, p.id, 0.9 FROM provenance p WHERE p.ref = $4`,
			m.Kind, m.Text, memScope, ref); err != nil {
			r.t.Fatalf("память не вставлена: %v", err)
		}
	}
}

// pushMention — сообщение с mention-сущностью (@Dio): в группе только
// reply/mention/command проходят prefilter в ход (§6.3).
func (s *stubAPI) pushMention(updateID, chatID int64, actorID int64, actorName, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgID++
	raw := fmt.Sprintf(
		`{"update_id":%d,"message":{"message_id":%d,"date":%d,"text":%q,`+
			`"entities":[{"type":"mention","offset":0,"length":4}],`+
			`"from":{"id":%d,"first_name":%q},"chat":{"id":%d,"type":"group"}}}`,
		updateID, s.msgID, time.Now().Unix(), text, actorID, actorName, chatID)
	s.updates = append(s.updates, telegram.Update{UpdateID: updateID, Raw: json.RawMessage(raw)})
}

// push — реплики актора одной пачкой (§6.2); в группе кейс шлёт упоминанием.
func (r *evalRun) push(c evalsuite.Case) {
	chatType := c.Setup.ChatType
	if chatType == "" {
		chatType = "private"
	}
	for i, msg := range c.Messages {
		if chatType == "group" {
			r.h.api.pushMention(int64(10+i), r.chatID, c.Setup.ActorID, c.Setup.Actor, msg)
			continue
		}
		r.h.api.pushAs(int64(10+i), r.chatID, chatType, c.Setup.ActorID, c.Setup.Actor, msg)
	}
}

// awaitTerminal — ждёт applied или skipped (терминальные состояния §12),
// затем опустошения outbox: сбор исхода не должен гонять доставку.
func (r *evalRun) awaitTerminal() {
	wait := r.wait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if queryInt(r.h, `SELECT count(*) FROM turns WHERE status IN ('applied','skipped')`) >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if queryInt(r.h, `SELECT count(*) FROM turns WHERE status IN ('applied','skipped')`) == 0 {
		r.t.Fatalf("ход кейса не дошёл до applied/skipped за %s", wait)
	}
	deadline = time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if queryInt(r.h, `SELECT count(*) FROM delivery_outbox
			WHERE status NOT IN ('sent','failed','cancelled','unknown')`) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.t.Fatalf("outbox кейса не опустел за %s", wait)
}

// collect — исход кейса: доставки, реакция, decision, tool-вызовы, пропуск.
func (r *evalRun) collect() {
	for _, call := range r.h.api.callsOf("sendMessage") {
		r.out.Messages = append(r.out.Messages, call.Text)
	}
	if rc := r.h.api.callsOf("setMessageReaction"); len(rc) > 0 {
		r.out.Reaction = rc[len(rc)-1].Emoji
	}
	r.out.Decision, _ = queryStringOK(r.h,
		`SELECT payload->'result'->>'decision' FROM event_streams WHERE type = 'model_output'
		 ORDER BY created_at DESC LIMIT 1`)
	// Вызовы инструментов с аргументами; результаты подмешиваются по call_id.
	results := map[string]string{}
	if res, err := r.h.pool.Query(context.Background(),
		`SELECT payload->>'call_id', coalesce(payload->>'result', payload->>'error')
		 FROM event_streams WHERE type = 'tool_result' ORDER BY created_at`); err == nil {
		for res.Next() {
			var id, resText string
			if err := res.Scan(&id, &resText); err == nil {
				results[id] = resText
			}
		}
		res.Close()
	}
	if calls, err := r.h.pool.Query(context.Background(),
		`SELECT payload->>'tool', payload->>'arguments', payload->>'call_id'
		 FROM event_streams WHERE type = 'tool_call' ORDER BY created_at`); err == nil {
		for calls.Next() {
			var name, args, id string
			if err := calls.Scan(&name, &args, &id); err == nil {
				r.out.ToolCalls = append(r.out.ToolCalls, eval.ToolCall{
					Name: name, Arguments: args, Result: cut(results[id], 400),
				})
			}
		}
		calls.Close()
	}
	if status, ok := queryStringOK(r.h, `SELECT status FROM turns ORDER BY id DESC LIMIT 1`); ok && status == "skipped" {
		r.out.Skipped = true
		if note, ok := queryStringOK(r.h,
			`SELECT payload->>'reason' FROM event_streams WHERE type='turn_skipped' ORDER BY created_at DESC LIMIT 1`); ok {
			r.out.SkipNote = note
		}
	}
	r.out.LLMCalls = r.h.modelCalls()
}

// runEvalCases — цикл набора: посев → пуш → ожидание → сборка → скоринг.
// newH строит приложение кейса (скриптовая или живая модель — решает вызывающий).
// Пишет отчёт в evals/results/<mode>.json, возвращает pass_rate.
func runEvalCases(t *testing.T, mode string, cases []evalsuite.Case,
	newH func(t *testing.T, c evalsuite.Case) *evalRun) float64 {
	type scoreReport struct {
		Name   string  `json:"name"`
		Passed bool    `json:"passed"`
		Value  float64 `json:"value"`
		Detail string  `json:"detail,omitempty"`
	}
	type caseReport struct {
		Name   string        `json:"name"`
		Passed bool          `json:"passed"`
		Scores []scoreReport `json:"scores"`
		Output string        `json:"output"`
		Detail string        `json:"detail,omitempty"`
	}
	var reports []caseReport
	passed, evaluated := 0, 0

	only := os.Getenv("EVAL_CASES")
	for _, c := range cases {
		if only != "" && !strings.Contains(","+only+",", ","+c.Name+",") {
			continue
		}
		r := newH(t, c)
		evaluated++
		r.seed(c)
		r.push(c)
		r.awaitTerminal()
		r.collect()

		scorers, err := evalsuite.BuildScorers(c, r.out)
		if err != nil {
			t.Fatalf("%v", err)
		}
		tr := evalsuite.Trace(c, r.out)
		cr := caseReport{Name: c.Name, Output: strings.Join(r.out.Messages, " | ")}
		if r.out.Skipped {
			cr.Detail = "ход пропущен: " + r.out.SkipNote
		}
		all := true
		for _, sc := range scorers {
			s := eval.ScoreWith(context.Background(), sc, tr)
			detail := ""
			if b, jerr := json.Marshal(s.Details); jerr == nil {
				detail = string(b)
			}
			cr.Scores = append(cr.Scores, scoreReport{Name: s.Name, Passed: s.Passed, Value: s.Value, Detail: detail})
			if !s.Passed {
				all = false
				t.Logf("    провален скорер %s: %s", s.Name, detail)
			}
		}
		cr.Passed = all && len(cr.Scores) > 0
		if cr.Passed {
			passed++
		}
		t.Logf("eval %-28s %s  выход=%q %s", c.Name, passFail(cr.Passed), cr.Output, cr.Detail)
		for _, tc := range r.out.ToolCalls {
			t.Logf("    tool %s args=%s результат=%s", tc.Name, tc.Arguments, tc.Result)
		}
		reports = append(reports, cr)
	}

	rate := 0.0
	if evaluated > 0 {
		rate = float64(passed) / float64(evaluated)
	}
	t.Logf("ИТОГ [%s]: %d/%d (pass_rate=%.2f)", mode, passed, evaluated, rate)

	if err := os.MkdirAll("../../evals/results", 0o755); err == nil {
		if blob, jerr := json.MarshalIndent(map[string]any{
			"mode": mode, "pass_rate": rate, "passed": passed, "total": evaluated,
			"cases": reports, "written_at": time.Now().UTC().Format(time.RFC3339),
		}, "", "  "); jerr == nil {
			_ = os.WriteFile(filepath.Join("../../evals/results", mode+".json"), blob, 0o644)
		}
	}
	return rate
}

// TestEvalSuiteOffline — детерминированный гейт: набор на скриптовой модели
// обязан проходить целиком (иначе сломан прогонщик или набор, не поведение).
func TestEvalSuiteOffline(t *testing.T) {
	cases := evalSuite(t)
	if rate := runEvalCases(t, "offline", cases, newEvalHarness); rate < 1.0 {
		t.Fatalf("офлайн-набор обязан проходить целиком: pass_rate=%.2f", rate)
	}
}

func evalSuite(t *testing.T) []evalsuite.Case {
	t.Helper()
	cases, err := evalsuite.LoadSuite(evalSuitePath)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(cases) == 0 {
		t.Fatalf("набор %s пуст", evalSuitePath)
	}
	return cases
}

// cut — хвост результата для отчёта (целый лежит в журнале).
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func passFail(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}
