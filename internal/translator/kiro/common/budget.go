package common

import "github.com/tidwall/gjson"

const (
	// DefaultThinkingBudget is the thinking budget injected into the Kiro
	// system prompt when the client does not supply one.
	DefaultThinkingBudget int64 = 16000

	// MaxThinkingBudget caps caller-supplied thinking budgets so the reasoning
	// output cannot crowd out tool results inside the Kiro context window.
	MaxThinkingBudget int64 = 32000
)

// ThinkingBudget returns the thinking budget to embed in the Kiro system
// prompt. A client-supplied budget (Claude-style thinking.budget_tokens, also
// accepted from OpenAI-compatible bodies) takes precedence over fallback; the
// result is capped at MaxThinkingBudget.
func ThinkingBudget(body []byte, fallback int64) int64 {
	budget := fallback
	if budget <= 0 {
		budget = DefaultThinkingBudget
	}
	if supplied := gjson.GetBytes(body, "thinking.budget_tokens"); supplied.Exists() {
		if value := supplied.Int(); value > 0 {
			budget = value
		}
	}
	if budget > MaxThinkingBudget {
		budget = MaxThinkingBudget
	}
	return budget
}
