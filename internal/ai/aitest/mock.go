// Package aitest provides a scripted AI provider for tests. It never makes
// network calls.
package aitest

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/quantamsage/ai-video-summarizer/internal/ai"
)

// Rule returns a response for requests whose prompt contains Match.
type Rule struct {
	Match    string
	Response string
	Err      error
	// Times limits how often the rule applies (0 = unlimited).
	Times int
	used  int
}

// Mock is a scripted ai.Provider. The first matching rule wins.
type Mock struct {
	mu    sync.Mutex
	Rules []*Rule
	Calls []ai.Request
}

func (m *Mock) Name() string  { return "mock" }
func (m *Mock) Model() string { return "mock-model" }

// On adds a rule and returns the mock for chaining.
func (m *Mock) On(match, response string) *Mock {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Rules = append(m.Rules, &Rule{Match: match, Response: response})
	return m
}

// OnceErr adds a rule that fails once with err.
func (m *Mock) OnceErr(match string, err error) *Mock {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Rules = append(m.Rules, &Rule{Match: match, Err: err, Times: 1})
	return m
}

func (m *Mock) Complete(ctx context.Context, req ai.Request) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls = append(m.Calls, req)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	for _, r := range m.Rules {
		if r.Times > 0 && r.used >= r.Times {
			continue
		}
		if strings.Contains(req.System+"\n"+req.Prompt, r.Match) {
			r.used++
			return r.Response, r.Err
		}
	}
	return "", errors.New("aitest: no rule matched prompt")
}

// CallCount returns the number of calls whose prompt contained match.
func (m *Mock) CallCount(match string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.Calls {
		if strings.Contains(c.System+"\n"+c.Prompt, match) {
			n++
		}
	}
	return n
}
