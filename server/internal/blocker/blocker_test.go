package blocker

import "testing"

func TestResolutionPending(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
		want     bool
	}{
		{name: "pending", metadata: `{"blocker_resolution_state":"resolver_pending"}`, want: true},
		{name: "resolved", metadata: `{"blocker_resolution_state":"resolved"}`},
		{name: "legacy", metadata: `{"blocked_reason":"missing credentials"}`},
		{name: "malformed", metadata: `{`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsResolutionPending([]byte(tc.metadata)); got != tc.want {
				t.Fatalf("IsResolutionPending() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolverHandoff(t *testing.T) {
	note := ResolverHandoff("Android SDK missing")
	if !IsResolverHandoff(note) {
		t.Fatalf("generated handoff was not recognized: %q", note)
	}
	if IsResolverHandoff("ordinary user handoff") {
		t.Fatal("ordinary handoff must not select the resolver workflow")
	}
}

func TestIsResolvedBy(t *testing.T) {
	metadata := []byte(`{"blocker_resolution_state":"resolved","blocker_resolver_agent_id":"leader-1"}`)
	if !IsResolvedBy(metadata, "leader-1") {
		t.Fatal("recorded resolver should be allowed to resume")
	}
	if IsResolvedBy(metadata, "other-agent") {
		t.Fatal("an unrelated agent must not bypass the self-loop guard")
	}
	if IsResolvedBy([]byte(`{"blocker_resolution_state":"resolver_pending","blocker_resolver_agent_id":"leader-1"}`), "leader-1") {
		t.Fatal("pending resolver must not resume the issue")
	}
}
