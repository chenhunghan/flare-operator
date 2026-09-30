# Scaling out generated kinds

How to add Cloudflare resources as CRD kinds in waves, with emulator coverage from day one.
The pieces:

- `generator.yaml` + `cmd/flaregen` (`internal/flaregen`): spec → Go types, CRDs, `generic.Descriptor`s.
- The **generic profile** of flarefake (`internal/fake/generic*.go`): a stateful CRUD store driven by
  a kind's descriptor and the pinned spec, for kinds with no hand-written profile. Everything it does
  is `UNVERIFIED` unless a recording backs it.
- The **per-kind suite** (`internal/generic/kindsuite`): runs every registered generated kind through
  the generic reconciler against envtest + in-process flarefake.

## Adding a kind in a wave

1. **Pick the resource.** `go run ./cmd/flaregen -list` shows every resource the model finds and
   why unsupported ones are unsupported. Good wave candidates are account-level CRUD resources with
   plain JSON bodies, no parent path parameters, and an ID the API returns (or the client chooses).
   Check the spec (create/get/update bodies, the ID's path parameter) and whether the Free plan can
   create it (the live recording pass must be able to).
2. **Add a `generator.yaml` entry** with `emulate: generic` and a `why` for every override (cite a
   recording or say `UNVERIFIED`). Typical overrides: `kind` (avoid generic names), `group` (the
   product must be a Go package name, e.g. `aigateway`), `idField` when the item path takes a
   field the generator cannot find (Vectorize: `name`), `defaultDeletionPolicy: Orphan` for
   data-bearing kinds, `tagResourceType` when Resource Tagging supports it.
3. **Regenerate:** `make generate-crds manifests chart-sync`. This writes `api/<group>/v1alpha1`,
   `config/crd/bases`, `internal/generic/descriptors/zz_generated.go`, the RBAC markers, and
   `internal/fake/zz_generated_generic.go` (the kinds the generic profile may serve). The kind is
   registered with the manager automatically (`internal/generic/kinds`). `make generate-check` and
   `make verify-generated` must be clean.
4. **Update the pinned expectations:** add the kind to `TestGeneratedUpToDate`,
   `TestRegistry` and `TestDescriptorsMatchEmulator` (`internal/flaregen`), give
   `TestDescriptorsAgainstFlarefake` (`internal/generic/descriptors`) a create/update sample, and
   add a manifest to `examples/` (`TestEveryKindHasAnExample`, validated by a dry-run create).
5. **Run the suite:** `go test ./internal/generic/kindsuite/ -run 'TestKinds/<Kind>' -v`. The suite
   synthesizes a minimal forProvider from the CRD schema (required fields; first enum value; a
   `flare-spike-*` name that satisfies pattern and length; the smallest allowed number; false),
   picks the first changeable UpdateField and Immutable field, and fails on any spec violation.
   When synthesis cannot express a valid body (flattened unions, cross-field rules), add
   `internal/generic/kindsuite/testdata/<Kind>.json` (`create`, optionally `update` and `immutable`;
   `"{name}"` becomes a unique name; `null` disables a step). The suite must pass on the generic
   profile before the wave merges.
6. **Live recording pass** (per `CLAUDE.md`: baseline inventory, `flare-spike-*` names, a ledger,
   verified cleanup, no plan upgrades): record create, get, list (empty and non-empty, with
   pagination), update, a duplicate create, get/delete of a missing item, and delete. Sanitize with
   `hack/sanitize_recordings.py` and scan for leaks.
7. **Replace UNVERIFIED with recorded behavior:**
   - Differences the generic profile can express go into `internal/fake/generic_quirks.go`, citing
     the recording (today: the recorded 404 error, result_info fields the spec declares but the API
     omits). Add the recordings to `TestGenericConformance` (`internal/fake/generic_test.go`).
   - Spec/API mismatches that affect the CRD go into `generator.yaml` overrides (`fields`,
     `writeOnly`, `immutable`, …) with the recording number.
   - When the product needs more than quirks (validation, error codes, async states, side effects),
     write a hand-written profile (`internal/fake/<product>.go`, every behavior cited), add its
     recordings to a `TestConformance` scenario, and drop `emulate: generic`. Hand-written profiles
     always take precedence: a generic kind whose routes a hand-written profile serves is skipped.

The first wave (2026-09-29) added `VectorizeIndex` (no update, client-chosen ID = name; fixture for
the `config` union), `SecretsStore` (no update; recorded list shape 0155 as a quirk) and `AIGateway`
(client-chosen `id`, PUT update, no name field). Hyperdrive has live recordings (0190–0217) and is a
good next candidate, but needs `writeOnly: [origin.password, origin.access_client_secret]` and a
fixture for its `origin` union, and its create validates the origin database connection, which
the generic profile cannot model.

The full-stack slice (2026-09-30) added `R2Bucket`, the first kind that needs the per-kind
extensions below: its jurisdiction is a request header, its storage class is updated through a
header, the create body's `storageClass` reads back as `storage_class`, and its CORS policy is a
sub-resource. No live recording exists; the only evidence besides the spec is wrangler 4.143.0's
source (SOURCED: the 10006 "bucket not found" code, the headers it sends). R2 must be enabled on
the account (possibly with a payment method) before the recording pass can create buckets.

## Per-kind extensions

Some resources need more than `generic.Descriptor` (a frozen contract) can say. Three
`generator.yaml` options cover the cases found so far. flaregen resolves them against the spec
into a `generic.Extension` on the kind's `descriptors.Entry`, which the generic reconciler
applies, and into the same fields of `fake.GenericKind` for the generic profile. The zero value
changes nothing, so kinds without them behave exactly as before.
`TestDescriptorsMatchEmulator` pins them.

| Option | What it means | Reconciler | Generic profile |
|---|---|---|---|
| `requestHeaders: [{header, field}]` (`sentOn: all`, the default) | A header parameter that the create operation declares carries a forProvider `field`. The field is added to forProvider with the header's schema, and is Immutable. | Sends the header on every request for the resource (create, get, list, update, delete, sub-resources), never on tag requests. Once the resource exists, a change is refused before any request: Synced=False, reason Immutable, compared with `status.atProvider`, because a GET with another value would not find the resource and the object would create a second one. The CRD gets a stricter CEL rule than other immutable fields: the *effective* value (unset = the header's spec default) cannot change, so setting, changing or removing it is refused. | The header partitions the collection (a resource made with one value is invisible with another) and is stored in the object's field when the item schema has one. |
| `requestHeaders: [{header, field, sentOn: update}]` | The update operation takes `field` in this header instead of a body. The field becomes an UpdateField. | The update moves the field from the body into the header; a body left empty is not sent. | An update request's header sets the stored field (or its `observedAs` name). An update route whose spec has no request body ignores any body sent. |
| `observedAs: {field: atProviderField}` | forProvider `field` is read back under another name. It is not derived write-only. | Drift, immutability and PUT bodies compare `field` with `atProvider.<atProviderField>`. | Create and update bodies store `field` under the other name. |
| `subResources: [{field, path, serverSet}]` | A fixed sub-path of the item with GET and PUT, such as `/cors`. forProvider `field` takes the PUT body's schema, atProvider `field` the GET result's. `serverSet` lists dotted paths into the GET result (list elements traversed, e.g. `rules.id`) of members the API may assign itself; each must exist in the GET schema. | GETs it after the item, into `atProvider.<field>` (404 = not configured, absent). When `forProvider.<field>` is set and is not the same document as what was read (compared exactly once empty members are dropped, not with the Covers rule used for item fields: the PUT replaces the whole document, so a member left out, such as a CORS rule's `exposeHeaders`, is one the PUT removes; a `serverSet` member is compared only where forProvider sets it), it PUTs it (an update, so `Update` must be allowed). An empty value (`{}`, or one with only empty lists such as `{"rules": []}`, which the generated types turn into `{}`) clears it: DELETE when the spec has one at the path (`Delete` in the descriptor), else PUT of the empty value; an absent or empty observed value is in sync. Unset means unmanaged. Nothing is deleted separately. Observe-only objects only read. | A document stored with the item: GET (404 when absent), PUT, DELETE when the spec has it; it is deleted with the item. |

A list result that is an object whose only member is the item array (R2's
`{"buckets": [...]}`) needs no option. `cfclient.ListAll` pages it like an array, and the
generic profile wraps its pages the way the spec's list result declares.

R2Bucket's lifecycle rules (`/lifecycle`), bucket locks (`/lock`) and similar documents are
further `subResources` with the same shape. Custom domains and event notifications have their own
IDs and need nested-resource support (G-gen).

## How the generic profile behaves

See the package comment in `internal/fake/generic.go`. In short: opt-in per kind
(`fake.Options.Generic`; `fake.GeneratedGenericKinds()` lists the `emulate: generic` kinds;
`cmd/flarefake -generic`, on by default when `-spec` is given); every request validated against the
spec (violations → 400/10001); results are the request body projected onto the spec's item schema
with defaults, server timestamps from the fake clock (precision from the spec example), account_id,
and zero values for absent required fields; IDs generated in the spec's shape or taken from the
request when the client chooses them; name conflicts → 409; PUT replaces, PATCH merges
(RFC 7386); DELETE → `null` or the spec's delete result; lists in creation order with page or
cursor pagination and exactly the result_info fields the spec declares (minus recorded quirks);
singletons start from a spec-derived default object. `New(Options{})` serves no generic route, so
`TestConformance`'s rule about recordings that hit emulated routes is unaffected.

## What the generic profile cannot model

- **Product validation and error codes.** It enforces only the spec schema. Real limits (name
  rules beyond the pattern, quotas such as one Secrets Store per account, plan entitlements,
  cross-field rules such as Hyperdrive's "mtls cannot be used with service_id" 0192) are absent.
  Error codes and messages are generic (404/7003, 409/10010, 400/10001) unless a quirk records
  the real one.
- **Uniqueness semantics.** Only NameField (and a client-chosen ID) is unique per account; whether
  the real API rejects duplicates, and with which status, is per product.
- **Server-computed values.** Anything not in the request, the spec default or a timestamp is a
  zero value or absent: derived fields (Vectorize `preset` → dimensions/metric), counters
  (`script_count`), generated secrets (Turnstile `secret`), status fields, etc.
- **Asynchronous state machines.** Resources that are provisioning/active/deleting (or report
  connections, health) are instantly "done".
- **Side effects and relations.** Creating one resource never creates, updates or validates
  another (Tunnels auto-create a virtual network, 0160; Hyperdrive connects to the origin
  database, 0194; bindings reference other resources).
- **Data inside a resource.** An R2 bucket holds no objects in the profile, so its delete is never
  refused as "not empty". Tests inject that refusal as a fault (409/10008, UNVERIFIED) to check
  the operator's `DeleteFailed` handling. Whether R2 is enabled on the account (a plan
  entitlement) is not modeled either.
- **Write-only fields** are dropped only when the spec marks them `writeOnly` or the descriptor
  lists them; a secret the spec forgets to mark is echoed back.
- **Update semantics beyond replace/merge.** PATCH endpoints with append/remove bodies (Gateway
  lists), partial PUTs, or updates that reset unrelated fields differently are not modeled; which
  fields a PUT resets is inferred from the PUT body schema.
- **List behavior.** Filters, search, ordering parameters and per-product default ordering are
  ignored (creation order); `per_page` defaults to the spec's default, else 20.
- **Non-JSON bodies** (multipart uploads, raw content, NDJSON) and non-envelope responses.
- **Envelope quirks** (null `errors`/`messages`, missing `messages`, 201/202/204 statuses,
  charset-less Content-Type, product rate-limit policies) — only the default envelope is emitted.
- **Integer or composite IDs**, nested resources (parent path parameters) and zone-scoped
  kinds (served, but the suite skips them without a zone), and singleton kinds in the suite.
- **Spec defects.** Where the pinned spec itself is wrong (e.g. an ambiguous oneOf that valid
  bodies match twice, as Hyperdrive's PATCH `caching`), the profile rejects valid requests; that
  needs a quirk or a hand-written profile.
