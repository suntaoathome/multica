package taskfailure

import "testing"

// TestClassifyCodexInitializeBaseline is an AI-190 Stage 1 (AI-195) QA
// characterization test. It pins the CURRENT classification of the four
// Codex app-server initialize/handshake failure classes AI-194 calls out
// (process_start_failed, initialize_timeout, daemon_runtime_unavailable,
// provider_capacity_or_rate_limit) as produced by codex.go and routed
// through Classify().
//
// It documents behavior, it does NOT assert desired behavior: the point is
// to make the free-text dependency executable so Stage 2 can change it
// deliberately (and see this test go red exactly where the taxonomy moves).
//
// Baseline finding: of the four classes, only provider_capacity_or_rate_limit
// gets a distinct machine-readable reason today, and only when the provider's
// "429"/"rate limit" text survives into Result.Error. The other three all
// collapse into agent_error.process_failure — there is no dedicated
// initialize_timeout / process_start_failed reason. Worse, the
// "codex initialize failed:" wrapper prefix itself contains the substring
// "initialize failed" (rule 13), so any wrapped init failure lacking a
// higher-priority marker is forced to process_failure and can never reach
// 'unknown'; classification hinges on incidental wrapper wording rather than
// the underlying cause. Daemon/runtime unavailability is a platform-side
// reason set by sweepers (runtime_offline / runtime_recovery), never derived
// from the agent error text by Classify. Error strings below are the literal
// forms codex.go emits: see CodexHandshakeTimeoutMarker and the
// `codex initialize failed: %v` wrapper in server/pkg/agent/codex.go.
func TestClassifyCodexInitializeBaseline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// in is the literal Result.Error text the codex backend surfaces.
		in string
		// want is the reason Classify returns TODAY (characterization).
		want Reason
		// note records the Stage 1 observation for the report.
		note string
	}{
		{
			name: "initialize_timeout (real production string)",
			in:   "codex initialize failed: codex app-server handshake timeout: initialize did not respond after 30s",
			want: ReasonAgentProcessFailure,
			note: "CONFLATED: the same reason a generic non-zero exit produces; " +
				"the 'handshake timeout' marker is invisible to the taxonomy. " +
				"No dedicated initialize_timeout reason exists.",
		},
		{
			name: "process exited before handshake",
			in:   "codex initialize failed: codex process exited",
			want: ReasonAgentProcessFailure,
			note: "CONFLATED: 'process exited'/'initialize failed' both land in " +
				"rule 13. No dedicated process_start_failed reason.",
		},
		{
			name: "retry suppressed: cleanup not confirmed",
			in:   "codex initialize failed: codex app-server handshake timeout: initialize did not respond after 30s; retry suppressed: process cleanup/reap not confirmed",
			want: ReasonAgentProcessFailure,
			note: "The recovery-action detail ('retry suppressed: ...') lives only " +
				"in free text; the reason is still the generic process_failure.",
		},
		{
			name: "daemon/runtime unavailable, wrapped as initialize failure",
			in:   "codex initialize failed: daemon runtime temporarily unavailable",
			want: ReasonAgentProcessFailure,
			note: "GAP + wrapper trap: Classify has no rule for daemon/runtime " +
				"unavailability, AND the 'codex initialize failed:' prefix itself " +
				"contains 'initialize failed' (rule 13), so any init failure lacking " +
				"a higher-priority marker is forced to process_failure and can never " +
				"reach 'unknown'. The real daemon/runtime signal is the platform-side " +
				"runtime_offline / runtime_recovery reasons set by sweepers, not this path.",
		},
		{
			name: "bare unavailable text (no init wrapper) would be unknown",
			in:   "daemon runtime temporarily unavailable",
			want: ReasonAgentUnknown,
			note: "Contrast to the wrapped case: without the 'initialize failed' " +
				"prefix the same cause falls through every rule to unknown. Proves " +
				"the classification hinges entirely on incidental wrapper wording.",
		},
		{
			name: "provider capacity/rate-limit surfaced during init",
			in:   "codex initialize failed: API Error: 429 rate limit exceeded",
			want: ReasonAgentProviderCapacityOrRateLimit,
			note: "DISTINGUISHABLE: rule 5 (capacity) precedes rule 13 (process), " +
				"so this is classified correctly — but ONLY if the provider's " +
				"'429'/'rate limit' text survives into Result.Error. A pure " +
				"handshake hang with no provider text degrades to process_failure.",
		},
		{
			name: "provider capacity 529 overloaded during init",
			in:   "codex initialize failed: API Error: 529 overloaded",
			want: ReasonAgentProviderCapacityOrRateLimit,
			note: "Same as above via the 529/overloaded branch of rule 5.",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tc.in); got != tc.want {
				t.Errorf("Classify(%q)\n  = %q\n want %q (baseline)\n note: %s",
					tc.in, got, tc.want, tc.note)
			}
		})
	}
}
