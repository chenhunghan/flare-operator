package harness

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// Discrepancy is a known mismatch between a real Cloudflare client and flarefake. Differential
// tests do not fix flarefake; they record each mismatch as a Discrepancy and run it with
// Check, which skips while the mismatch reproduces.
type Discrepancy struct {
	// ID is stable and unique across the suite, e.g. "WR-QUEUE-LIST-COUNTS".
	ID string
	// Client is the client and pinned version that shows it, e.g. "wrangler@4.143.0".
	Client string
	// Summary says what the client expects and what flarefake does instead.
	Summary string
	// Evidence cites the client code that expects the behaviour:
	// "<repo>@<version or sha>:<path>#L<n>".
	Evidence string
}

func (d Discrepancy) String() string {
	return fmt.Sprintf("%s (%s): %s [evidence: %s]", d.ID, d.Client, d.Summary, d.Evidence)
}

// Check runs check in the subtest "discrepancy/<ID>". check returns an error while flarefake
// still behaves differently from what the client needs; the subtest then skips with the
// reason "discrepancy: …", the evidence and the observed error. When check returns nil the
// discrepancy no longer reproduces and the subtest fails, so whoever fixed flarefake removes
// the entry (and any shim registered for it) and lets the scenario test the fix.
func (d Discrepancy) Check(t *testing.T, check func() error) {
	t.Helper()
	t.Run("discrepancy/"+d.ID, func(t *testing.T) {
		err := check()
		if err == nil {
			t.Fatalf("discrepancy %s no longer reproduces: remove it and its shim from the test so the scenario covers the fixed behaviour.\n%s", d.ID, d)
		}
		t.Skipf("discrepancy: %s\n  client:   %s\n  evidence: %s\n  observed: %s", d.Summary, d.Client, d.Evidence, oneLine(err.Error()))
	})
}

// SpecDefect is a place where the pinned OpenAPI spec rejects a request the live API accepts
// (proven by a recording). flarefake only journals these (it answers normally unless
// -reject-schema-violations is set), so a client that sends such a request is right and the
// spec is wrong; they are not flarefake discrepancies.
type SpecDefect struct {
	Match  *regexp.Regexp // matched against a SchemaViolations entry
	Reason string
}

// KnownSpecDefects lists the pinned-spec defects real clients hit. The same defects are
// allowlisted in internal/generic/descriptors/emulator_test.go and test/e2e.
var KnownSpecDefects = []SpecDefect{
	{regexp.MustCompile(`^DELETE /accounts/[^/]+/storage/kv/namespaces/[^/:]+: request body has an error: value is required but missing$`),
		"the spec requires a body on KV namespace DELETE; the live API deletes without one (recording 0013)"},
	{regexp.MustCompile(`^GET /accounts/[^/]+/d1/database/[^/:]+: parameter "database_id" in path has an error: input matches more than one oneOf schemas$`),
		"D1 database_id is an ambiguous oneOf in the spec (recording 0020)"},
	{regexp.MustCompile(`^PUT /accounts/[^/]+/cfd_tunnel/[^/]+/configurations: request body has an error: doesn't match schema: Error at "/config/ingress/\d+/hostname": property "hostname" is missing$`),
		"the spec requires hostname on every ingress rule; the live API accepts and returns a catch-all rule without one (recordings 0045, 0046)"},
}

// UnexplainedSchemaViolations returns f.SchemaViolations() minus KnownSpecDefects and minus
// the entries in known (exact matches, each covered by a Discrepancy of the calling test).
func (f *Fake) UnexplainedSchemaViolations(known ...string) []string {
	var out []string
next:
	for _, v := range f.SchemaViolations() {
		for _, d := range KnownSpecDefects {
			if d.Match.MatchString(v) {
				continue next
			}
		}
		for _, k := range known {
			if k == v {
				continue next
			}
		}
		out = append(out, v)
	}
	return out
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return strings.ReplaceAll(s, "\n", " | ")
}
