package fake

// genericQuirk corrects the generic profile (generic.go) for one kind where a recording shows
// the real API differs from what the spec implies. Every entry must cite its evidence (a
// recording, else SOURCED from an official client, per CLAUDE.md's evidence tiers); this
// is where a live recording pass (docs/generator-scaleout.md) moves a kind's behavior from
// UNVERIFIED to recorded, until the kind is worth a hand-written profile.
type genericQuirk struct {
	// notFound is the recorded (or SOURCED) error of a GET/PUT/PATCH/DELETE on a missing item.
	notFound *APIError
	// resultInfoOmit lists result_info fields the spec declares but the list never returns.
	resultInfoOmit []string
}

// genericQuirks is keyed by "<group>/<Kind>".
var genericQuirks = map[string]genericQuirk{
	// 0155: GET /secrets_store/stores answers result_info {page, per_page, count, total_count};
	// the spec also declares total_pages.
	"secretsstore.cloudflare.flare.dev/SecretsStore": {resultInfoOmit: []string{"total_pages"}},

	// VectorizeIndex: the list without result_info (0154) is what the spec says; no quirk.

	// R2Bucket: a missing bucket answers error code 10006. SOURCED (relies), no recording:
	// cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/helpers/provision-bindings.ts
	// (R2Handler.isConnectedToExistingResource treats an APIError with code 10006 from
	// GET …/r2/buckets/{name} as "the bucket does not exist"; wrangler 4.143.0
	// wrangler-dist/cli.js#L170260). The HTTP status (404) and the message are UNVERIFIED.
	"r2.cloudflare.flare.dev/R2Bucket": {notFound: &APIError{Code: 10006, Message: "The specified bucket does not exist."}},
}
