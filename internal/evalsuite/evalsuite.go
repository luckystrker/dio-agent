// Package evalsuite — поведенческие эвалы персонажа (§21.2, эталоны 8.2).
//
// Кейс — не промпт агенту, а диалоговая ситуация: посев памяти/отношений,
// сообщения от актора, ожидание хода приложения. Исход (доставленные сообщения,
// реакция, decision, вызовы инструментов) собирается в ago/eval.Trace и
// оценивается скорерами: готовыми из ago/eval (correctness, tools_called) и
// своими замыканиями (silence_ok, not_contains, contains_any, decision_in).
// Отчёт — pass_rate в духе ago/eval.RunResult + JSONL-артефакт прогона.
package evalsuite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/luckystrker/ago/eval"
)

// SeedMemory — запись памяти, засеянная до кейса (provenance назначает
// посев, скоуп — скоуп чата кейса).
type SeedMemory struct {
	Kind string `json:"kind"` // claim | experience | thought | preference
	Text string `json:"text"`
}

// SeedRelationship — числовое отношение к актору до кейса (§7.3).
type SeedRelationship struct {
	Familiarity float64 `json:"familiarity"`
	Affinity    float64 `json:"affinity"`
	Trust       float64 `json:"trust"`
}

// Setup — состояние мира до кейса.
type Setup struct {
	Scope        string            `json:"scope,omitempty"`
	ChatType     string            `json:"chat_type,omitempty"` // private | group
	Actor        string            `json:"actor,omitempty"`
	ActorID      int64             `json:"actor_id,omitempty"`
	Relationship *SeedRelationship `json:"relationship,omitempty"`
	Memory       []SeedMemory      `json:"memory,omitempty"`
}

// Expect — критерии успеха кейса. Пустой Expect кейс не оценит (в ago/eval
// кейс без скореров не считается пройденным) — это ошибка набора.
type Expect struct {
	DecisionIn  []string `json:"decision_in,omitempty"`
	SilenceOK   bool     `json:"silence_ok,omitempty"`
	ContainsAny []string `json:"contains_any,omitempty"`
	ContainsAll []string `json:"contains_all,omitempty"`
	NotContains []string `json:"not_contains,omitempty"`
	ToolsAny    []string `json:"tools_any,omitempty"`
}

// Case — один кейс набора: диалоговая ситуация + критерии.
type Case struct {
	Name     string   `json:"name"`
	Messages []string `json:"messages"` // реплики актора одной пачкой (§6.2)
	Setup    Setup    `json:"setup"`
	Expect   Expect   `json:"expect"`
	// Scripted — офлайн-режим: ответы скриптового дубля модели по очереди
	// (тело TurnResult). В живом прогоне игнорируется.
	Scripted []string `json:"scripted,omitempty"`
}

// Outcome — исход кейса, собранный прогонщиком из приложения.
type Outcome struct {
	Messages  []string // доставленные тексты (по порядку)
	Reaction  string   // поставленная реакция ("" — нет)
	Decision  string   // decision из model_output ("" — ход не дошёл)
	Skipped   bool     // ход терминально пропущен
	SkipNote  string
	ToolCalls []eval.ToolCall
	LLMCalls  int
}

// LoadSuite читает набор кейсов из JSONL (по одному кейсу на строку;
// пустые строки и // — комментарии).
func LoadSuite(path string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("evalsuite: набор не читается: %w", err)
	}
	var cases []Case
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("evalsuite: %s: строка %d: %w", path, i+1, err)
		}
		if c.Name == "" || len(c.Messages) == 0 {
			return nil, fmt.Errorf("evalsuite: %s: строка %d: нужны name и messages", path, i+1)
		}
		cases = append(cases, c)
	}
	return cases, nil
}

// --- собственные скореры: замыкания на исход кейса --------------------------
// Сигнатуры — eval.Scorer, чтобы кейсы агрегировались единообразно.

type silenceScorer struct{ ok bool }

func (s silenceScorer) Name() string { return "silence_ok" }

func (s silenceScorer) Score(_ context.Context, t *eval.Trace) (eval.Score, error) {
	silent := t.FinalResponse == ""
	return boolScore(s.Name(), s.ok == silent, map[string]any{
		"silent": silent, "silence_ok": s.ok,
	}), nil
}

type decisionScorer struct {
	allowed []string
	actual  string
}

func (s decisionScorer) Name() string { return "decision_in" }

func (s decisionScorer) Score(_ context.Context, _ *eval.Trace) (eval.Score, error) {
	ok := false
	for _, d := range s.allowed {
		if d == s.actual {
			ok = true
		}
	}
	return boolScore(s.Name(), ok, map[string]any{"actual": s.actual, "allowed": s.allowed}), nil
}

type notContainsScorer struct{ phrase, joined string }

func (s notContainsScorer) Name() string { return "not_contains" }

func (s notContainsScorer) Score(_ context.Context, _ *eval.Trace) (eval.Score, error) {
	leak := strings.Contains(strings.ToLower(s.joined), strings.ToLower(s.phrase))
	return boolScore(s.Name(), !leak, map[string]any{"phrase": s.phrase, "leaked": leak}), nil
}

type containsAnyScorer struct {
	phrases []string
	joined  string
}

func (s containsAnyScorer) Name() string { return "contains_any" }

func (s containsAnyScorer) Score(_ context.Context, _ *eval.Trace) (eval.Score, error) {
	low := strings.ToLower(s.joined)
	for _, p := range s.phrases {
		if strings.Contains(low, strings.ToLower(p)) {
			return boolScore(s.Name(), true, map[string]any{"hit": p, "phrases": s.phrases}), nil
		}
	}
	return boolScore(s.Name(), false, map[string]any{"phrases": s.phrases}), nil
}

// BuildScorers собирает скореры кейса: готовые ago/eval (correctness по
// contains-all, tools_called) + замыкания на исход. Пустой expect — ошибка набора.
func BuildScorers(c Case, out Outcome) ([]eval.Scorer, error) {
	joined := strings.Join(out.Messages, "\n")
	e := c.Expect
	var scorers []eval.Scorer
	if e.SilenceOK {
		scorers = append(scorers, silenceScorer{ok: true})
	}
	if len(e.DecisionIn) > 0 {
		scorers = append(scorers, decisionScorer{allowed: e.DecisionIn, actual: out.Decision})
	}
	if len(e.ContainsAny) > 0 {
		scorers = append(scorers, containsAnyScorer{phrases: e.ContainsAny, joined: joined})
	}
	for _, p := range e.ContainsAll {
		scorers = append(scorers, eval.Correctness{Expected: p, Mode: eval.MatchContains})
	}
	for _, p := range e.NotContains {
		scorers = append(scorers, notContainsScorer{phrase: p, joined: joined})
	}
	if len(e.ToolsAny) > 0 {
		scorers = append(scorers, eval.ToolsCalled{Names: e.ToolsAny, Mode: eval.CallAny})
	}
	if len(scorers) == 0 {
		return nil, fmt.Errorf("evalsuite: кейс %q: пустой expect — кейс ничем оценён не будет", c.Name)
	}
	return scorers, nil
}

// Trace собирает ago/eval.Trace из исхода кейса: скореры видят ровно его.
// LLMCalls заполняется числом вызовов без деталей — correctness требует
// «модель отвечала», детали не нужны ни одному скореру набора.
func Trace(c Case, out Outcome) *eval.Trace {
	return &eval.Trace{
		AgentName:     "dio-eval",
		FinalResponse: strings.Join(out.Messages, "\n"),
		ToolCalls:     out.ToolCalls,
		LLMCalls:      make([]eval.LLMCall, max0(out.LLMCalls)),
	}
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func boolScore(name string, passed bool, details map[string]any) eval.Score {
	v := 0.0
	if passed {
		v = 1.0
	}
	return eval.Score{Name: name, Value: v, Passed: passed, Details: details}
}
