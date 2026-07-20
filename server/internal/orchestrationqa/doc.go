// Package orchestrationqa is the QA acceptance-matrix harness for the Handoff
// self-iteration scheduling closed loop (AI-102 / AI-106, Stage 1).
//
// It is a test-only package: it ships no production logic (QA may add tests,
// fixtures, and test tooling only — never production code that makes a test
// pass). The package holds one non-test file (this doc) so `go build ./...`
// includes it, plus:
//
//   - fixtures_test.go          reusable DB fixtures + invariant assertions
//   - acceptance_matrix_test.go the M1..M10 acceptance matrix as subtests
//
// The matrix, its expected event sequences, and the database invariants each
// row asserts are documented in docs/qa/self-iteration-acceptance-matrix.md;
// the deterministic fault-injection recipes (no time.Sleep) are in
// docs/qa/self-iteration-fault-injection-plan.md.
//
// Execution model: every test is an integration test gated on DATABASE_URL and
// skips cleanly when Postgres is not reachable, mirroring the established
// pattern in internal/scheduler and internal/handler. Rows that target
// behavior not yet built (next-round generation, single-active-author fence,
// derived liveness) are t.Skip("GAP …") so the suite stays green while marking
// the Stage 2 targets; Stage 3 (AI-109) un-skips and asserts them against the
// fixed backend.
//
// Boundary with AI-104: AI-104 owns the reproduction tests that prove the
// current defects FAIL. This package owns the acceptance grid + the reusable
// seeders/assertions Stage 3 re-runs to prove closure. Shared helpers here are
// intentionally dependency-light (raw pgxpool + SQL) so neither task's test
// wiring couples to the other.
package orchestrationqa
