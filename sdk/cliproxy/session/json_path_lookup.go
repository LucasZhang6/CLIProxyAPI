package session

import (
	"strings"

	"github.com/tidwall/gjson"
)

// firstNormalizedPathValue returns the first non-empty normalized session
// candidate among paths. Paths may be top-level keys or dotted paths with at
// most two segments (for example metadata.parent_thread_id). One pass over the
// root objects avoids rescanning a large Responses body for every candidate key.
func firstNormalizedPathValue(roots ...gjson.Result) string {
	return firstNormalizedPathValueAmong(sessionParentCandidatePaths, roots...)
}

func firstNormalizedPathValueAmong(paths []string, roots ...gjson.Result) string {
	if len(paths) == 0 || len(roots) == 0 {
		return ""
	}
	needed := make(map[string]struct{}, 8)
	for _, path := range paths {
		head, _, _ := strings.Cut(path, ".")
		if head != "" {
			needed[head] = struct{}{}
		}
	}
	for _, root := range roots {
		if !root.Exists() {
			continue
		}
		top := make(map[string]gjson.Result, len(needed))
		root.ForEach(func(key, value gjson.Result) bool {
			if _, ok := needed[key.Str]; ok {
				top[key.Str] = value
			}
			return true
		})
		for _, path := range paths {
			head, rest, nested := strings.Cut(path, ".")
			value, ok := top[head]
			if !ok {
				continue
			}
			if nested {
				value = value.Get(rest)
			}
			if candidate := normalizedSessionCandidate(value.String()); candidate != "" {
				return candidate
			}
		}
	}
	return ""
}

// sessionParentCandidatePaths is the ordered parent-id search list used by
// ExtractSessionInfo. Keeping it package-scoped avoids rebuilding the slice on
// every large Codex request.
var sessionParentCandidatePaths = []string{
	"parent_session_id", "parentSessionId", "parentSessionID",
	"parent_thread_id", "parentThreadId", "parentThreadID",
	"forked_from_thread_id", "forked_from_id",
	"parent_conversation_id", "parentConversationId", "parentConversationID",
	"parent_id", "parentId", "parentID",
	"parent_task_id", "parentTaskId", "parentTaskID",
	"parent_action_id", "parentActionId", "parentActionID",
	"parent_session", "parentSession",
	"parent_subagent_id", "parentSubagentId",
	"forkSource.sessionId", "fork_source.session_id",
	"previousSessionId", "previous_session_id",
	"metadata.parent_session_id", "metadata.parentSessionId", "metadata.parentSessionID",
	"metadata.parent_thread_id", "metadata.parentThreadId",
	"metadata.forked_from_thread_id", "metadata.forked_from_id",
	"metadata.parent_id", "metadata.parentId", "metadata.parentID",
	"metadata.parent_task_id", "metadata.parentTaskId", "metadata.parentTaskID",
	"metadata.parent_action_id", "metadata.parentActionId",
	"metadata.parent_subagent_id", "metadata.parentSubagentId",
	"metadata.parent_session", "metadata.parentSession",
	"metadata.parent_agent_id", "metadata.parentAgentId",
	"metadata.forkSource.sessionId", "metadata.previousSessionId",
	"extra_body.parent_session_id", "extra_body.parentSessionId", "extra_body.parentSessionID",
	"extra_body.parent_thread_id", "extra_body.parentThreadId",
	"extra_body.forked_from_thread_id", "extra_body.forked_from_id",
	"extra_body.parent_id", "extra_body.parentId", "extra_body.parentID",
	"extra_body.parent_task_id", "extra_body.parentTaskId",
	"extra_body.parent_action_id", "extra_body.parentActionId",
	"extra_body.parent_subagent_id", "extra_body.parentSubagentId",
	"extra_body.parent_session", "extra_body.parentSession",
}

var sessionForkCandidatePaths = []string{
	"forked_from_thread_id", "forked_from_id",
	"metadata.forked_from_thread_id", "metadata.forked_from_id",
	"extra_body.forked_from_thread_id", "extra_body.forked_from_id",
}

var sessionThreadCandidatePaths = []string{
	"thread_id", "threadId", "metadata.thread_id",
}
