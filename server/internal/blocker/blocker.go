package blocker

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	ResolutionStateKey = "blocker_resolution_state"
	ResolverAgentKey   = "blocker_resolver_agent_id"
	ResolverSquadKey   = "blocker_resolver_squad_id"
	ReasonKey          = "blocked_reason"

	ResolutionPending  = "resolver_pending"
	ResolutionResolved = "resolved"
	ResolutionTerminal = "terminal"

	resolverHandoffPrefix = "multica:blocker-resolver:v1"
)

// ResolverHandoff builds the internal handoff that selects the dedicated
// blocker-resolution workflow in the daemon brief.
func ResolverHandoff(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "The assigned agent reported a blocker without a structured reason. Read the latest comments for details."
	}
	return fmt.Sprintf("%s\nResolve the assigned agent's blocker before resuming the issue. Reported blocker: %s", resolverHandoffPrefix, reason)
}

// IsResolverHandoff reports whether a task was created by the blocker
// coordinator rather than by an ordinary assignment or promotion.
func IsResolverHandoff(note string) bool {
	return strings.HasPrefix(strings.TrimSpace(note), resolverHandoffPrefix)
}

// IsResolutionPending reports whether a blocked issue has an active automatic
// resolver. It intentionally fails open to false for malformed legacy data.
func IsResolutionPending(metadata []byte) bool {
	values := parseMetadata(metadata)
	state, _ := values[ResolutionStateKey].(string)
	return state == ResolutionPending
}

// IsResolvedBy reports whether actorID is the resolver that explicitly marked
// this blocker resolved. The issue trigger uses it to distinguish a legitimate
// cross-agent resume from a same-issue agent self-loop.
func IsResolvedBy(metadata []byte, actorID string) bool {
	values := parseMetadata(metadata)
	state, _ := values[ResolutionStateKey].(string)
	resolverID, _ := values[ResolverAgentKey].(string)
	return state == ResolutionResolved && resolverID != "" && resolverID == actorID
}

func parseMetadata(metadata []byte) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		return nil
	}
	return values
}

// Reason returns the agent-reported blocker reason from issue metadata.
func Reason(metadata []byte) string {
	if len(metadata) == 0 {
		return ""
	}
	values := parseMetadata(metadata)
	reason, _ := values[ReasonKey].(string)
	return strings.TrimSpace(reason)
}
