package fake

// genericQuirk corrects the generic profile (generic.go) for one kind where a recording shows
// the real API differs from what the spec implies. Every entry must cite its recording; this
// is where a live recording pass (docs/generator-scaleout.md) moves a kind's behavior from
// UNVERIFIED to recorded, until the kind is worth a hand-written profile.
type genericQuirk struct {
	// notFound is the recorded error of a GET/PUT/PATCH/DELETE on a missing item.
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
}
