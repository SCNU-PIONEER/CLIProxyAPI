package common

import "strings"

// DefaultAgentTaskType is the task type used when the auth record does not
// pin one or carries an unsupported value.
const DefaultAgentTaskType = "vibe"

// supportedAgentTaskTypes mirrors the Kiro API task types documented for the
// provider configuration ("vibe", "dev", "chat").
var supportedAgentTaskTypes = map[string]struct{}{
	"vibe": {},
	"dev":  {},
	"chat": {},
}

// NormalizeAgentTaskType returns a protocol-valid Kiro task type. Unknown or
// empty values fall back to DefaultAgentTaskType.
func NormalizeAgentTaskType(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if _, ok := supportedAgentTaskTypes[value]; ok {
		return value
	}
	return DefaultAgentTaskType
}

// AgentTaskType resolves the task type carried by builder metadata. The auth
// layer stores the configured value under the agent_task_type key.
func AgentTaskType(metadata map[string]any) string {
	if raw, ok := metadata["agent_task_type"].(string); ok {
		return NormalizeAgentTaskType(raw)
	}
	return DefaultAgentTaskType
}
