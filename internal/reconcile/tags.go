package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"flare.dev/operator/internal/cfclient"
)

// OwnerTagKey is the Resource Tagging key that records which Kubernetes object manages a
// Cloudflare resource (docs/cloudflare-service-catalog.md, "Ownership").
const OwnerTagKey = "flare.dev/owner"

// OwnerValue formats the owner tag value <cluster>/<namespace>/<name>.
func OwnerValue(cluster, namespace, name string) string {
	return cluster + "/" + namespace + "/" + name
}

// TagTarget identifies an account-level resource for Resource Tagging.
type TagTarget struct {
	// Type is the API's resource_type, e.g. kv_namespace, queue, d1_database, cloudflared_tunnel.
	Type string
	// ID is the Cloudflare resource ID.
	ID string
	// WorkerID is required for worker_version resources only.
	WorkerID string
}

// Tagger maintains the ownership tag. Implementations must be idempotent: an EnsureOwner on an
// already-tagged resource must make no write. Callers skip tagging when the management
// policies forbid writes (Policies.CanWrite).
type Tagger interface {
	// EnsureOwner makes the owner tag equal owner, preserving all other tags. If the resource
	// carries a different owner it returns an *OwnershipConflictError and writes nothing.
	EnsureOwner(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget, owner string) error
	// RemoveOwner removes the owner tag if (and only if) it equals owner.
	RemoveOwner(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget, owner string) error
}

// OwnershipConflictError: the resource is tagged as owned by someone else.
type OwnershipConflictError struct {
	Target        TagTarget
	Owner, Wanted string
}

func (e *OwnershipConflictError) Error() string {
	return fmt.Sprintf("%s %s is owned by %q (tag %s), not %q", e.Target.Type, e.Target.ID, e.Owner, OwnerTagKey, e.Wanted)
}

// NoopTagger disables ownership tagging.
type NoopTagger struct{}

func (NoopTagger) EnsureOwner(context.Context, cfclient.Client, string, TagTarget, string) error {
	return nil
}
func (NoopTagger) RemoveOwner(context.Context, cfclient.Client, string, TagTarget, string) error {
	return nil
}

// ResourceTagger implements Tagger over /accounts/{account_id}/tags.
//
// PUT replaces the whole tag map, so every change is GET → merge → PUT. The PUT (and the DELETE
// that removes the last tag) carries If-Match with the etag from the GET; a 412 means someone
// changed the tags in between, so the tagger re-reads, re-merges and retries (at most
// MaxConflictRetries times, then the 412 is returned for a requeue).
//
// Reading the tags of a never-tagged resource answers HTTP 500 (docs/cloudflare-service-
// catalog.md; UNVERIFIED, no recording), which is also what a transient failure looks like.
// A merge built from such an ambiguous empty read could drop other tags, so the tagger never
// writes from it directly. After the GET has failed with 500 twice (cfclient retries it once),
// it asks the tag index, GET /tags/resources?type=<type>&id=<id>:
//   - the resource is listed with tags → the 500 was transient; the listed tags and etag are
//     used instead (the If-Match on the write rejects them if the index was stale);
//   - the resource is not listed → it is treated as never tagged, and the first PUT is sent
//     without If-Match (there is no etag to send);
//   - the listing fails → the original 500 is returned and nothing is written.
//
// Residual risk (UNVERIFIED): the index is eventually consistent, so a transient GET 500 on a
// resource tagged moments ago by someone else, not yet indexed, could still lose that tag.
type ResourceTagger struct {
	// Key overrides OwnerTagKey (tests).
	Key string
	// MaxConflictRetries bounds re-GET/merge/retry rounds after a 412 (0 → DefaultConflictRetries;
	// negative → no retry).
	MaxConflictRetries int
}

// DefaultConflictRetries is ResourceTagger's default for MaxConflictRetries.
const DefaultConflictRetries = 3

func (r ResourceTagger) key() string {
	if r.Key != "" {
		return r.Key
	}
	return OwnerTagKey
}

func (r ResourceTagger) conflictRetries() int {
	switch {
	case r.MaxConflictRetries < 0:
		return 0
	case r.MaxConflictRetries == 0:
		return DefaultConflictRetries
	}
	return r.MaxConflictRetries
}

type tagsResult struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	WorkerID string            `json:"worker_id"`
	Tags     map[string]string `json:"tags"`
	ETag     string            `json:"etag"`
}

// tagState is one read of a resource's tags.
type tagState struct {
	tags   map[string]string // never nil
	etag   string            // "" when the resource was never tagged
	tagged bool
}

// Get returns the resource's tags; tagged=false when the resource was never tagged (a 500 that
// the tag index confirms, see ResourceTagger).
func (r ResourceTagger) Get(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget) (tags map[string]string, tagged bool, err error) {
	st, err := r.read(ctx, cf, accountID, t)
	if err != nil {
		return nil, false, err
	}
	return st.tags, st.tagged, nil
}

func (r ResourceTagger) read(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget) (*tagState, error) {
	q := url.Values{"resource_type": {t.Type}, "resource_id": {t.ID}}
	if t.WorkerID != "" {
		q.Set("worker_id", t.WorkerID)
	}
	// One cfclient retry (also for the index): a second 500 in a row is what "never tagged"
	// looks like.
	ctx = cfclient.WithMaxRetries(cfclient.WithoutCache(ctx), 1)
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: tagsPath(accountID), Query: q})
	if err != nil {
		if ae, ok := cfclient.AsAPIError(err); ok && ae.Status == http.StatusInternalServerError {
			return r.readFromIndex(ctx, cf, accountID, t, err)
		}
		return nil, err
	}
	var res tagsResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, fmt.Errorf("decode tags of %s %s: %w", t.Type, t.ID, err)
	}
	if res.Tags == nil {
		res.Tags = map[string]string{}
	}
	return &tagState{tags: res.Tags, etag: res.ETag, tagged: true}, nil
}

// readFromIndex disambiguates a GET that answered 500 (see ResourceTagger). getErr is returned
// when the index cannot be read.
func (r ResourceTagger) readFromIndex(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget, getErr error) (*tagState, error) {
	q := url.Values{"type": {t.Type}, "id": {t.ID}}
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: tagsPath(accountID) + "/resources", Query: q})
	if err != nil {
		return nil, fmt.Errorf("reading tags of %s %s failed (%w) and the tag index could not confirm it is untagged: %v", t.Type, t.ID, getErr, err)
	}
	var list []tagsResult
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		return nil, fmt.Errorf("reading tags of %s %s failed (%w); decode tag index: %v", t.Type, t.ID, getErr, err)
	}
	for _, e := range list {
		if e.ID != t.ID || (e.Type != "" && e.Type != t.Type) || (t.WorkerID != "" && e.WorkerID != "" && e.WorkerID != t.WorkerID) {
			continue
		}
		if len(e.Tags) == 0 {
			continue
		}
		if e.ETag == "" {
			// Without an etag the write could not be guarded; treat the 500 as transient.
			return nil, fmt.Errorf("reading tags of %s %s failed but the tag index lists tags for it; retry: %w", t.Type, t.ID, getErr)
		}
		return &tagState{tags: e.Tags, etag: e.ETag, tagged: true}, nil
	}
	if resp.ResultInfo != nil && resp.ResultInfo.Cursor != "" {
		// More pages although the listing is filtered to one ID: do not guess.
		return nil, fmt.Errorf("reading tags of %s %s failed and the tag index answer was paginated: %w", t.Type, t.ID, getErr)
	}
	return &tagState{tags: map[string]string{}}, nil
}

func tagsPath(accountID string) string { return "/accounts/" + accountID + "/tags" }

func ifMatch(etag string) http.Header {
	if etag == "" {
		return nil
	}
	return http.Header{"If-Match": {etag}}
}

func targetBody(t TagTarget) map[string]any {
	body := map[string]any{"resource_type": t.Type, "resource_id": t.ID}
	if t.WorkerID != "" {
		body["worker_id"] = t.WorkerID
	}
	return body
}

func (r ResourceTagger) put(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget, tags map[string]string, etag string) error {
	body := targetBody(t)
	body["tags"] = tags
	_, err := cf.Do(ctx, cfclient.Request{Method: http.MethodPut, Path: tagsPath(accountID), Body: body, Header: ifMatch(etag)})
	return err
}

func (r ResourceTagger) deleteAll(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget, etag string) error {
	_, err := cf.Do(ctx, cfclient.Request{Method: http.MethodDelete, Path: tagsPath(accountID), Body: targetBody(t), Header: ifMatch(etag)})
	return err
}

func isPreconditionFailed(err error) bool {
	ae, ok := cfclient.AsAPIError(err)
	return ok && ae.Status == http.StatusPreconditionFailed
}

// modify runs read → change → write, retrying the whole round after a 412. change returns
// write=false to stop without writing.
func (r ResourceTagger) modify(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget,
	change func(st *tagState) (write bool, err error)) error {
	for attempt := 0; ; attempt++ {
		st, err := r.read(ctx, cf, accountID, t)
		if err != nil {
			return err
		}
		write, err := change(st)
		if err != nil || !write {
			return err
		}
		if len(st.tags) == 0 {
			err = r.deleteAll(ctx, cf, accountID, t, st.etag)
		} else {
			err = r.put(ctx, cf, accountID, t, st.tags, st.etag)
		}
		if isPreconditionFailed(err) && attempt < r.conflictRetries() {
			continue
		}
		return err
	}
}

// EnsureOwner implements Tagger.
func (r ResourceTagger) EnsureOwner(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget, owner string) error {
	return r.modify(ctx, cf, accountID, t, func(st *tagState) (bool, error) {
		cur, has := st.tags[r.key()]
		switch {
		case has && cur == owner:
			return false, nil
		case has && cur != "":
			return false, &OwnershipConflictError{Target: t, Owner: cur, Wanted: owner}
		}
		st.tags[r.key()] = owner
		return true, nil
	})
}

// RemoveOwner implements Tagger. Removing the last tag sends DELETE (204) instead of an empty PUT.
func (r ResourceTagger) RemoveOwner(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget, owner string) error {
	err := r.modify(ctx, cf, accountID, t, func(st *tagState) (bool, error) {
		if !st.tagged || st.tags[r.key()] != owner {
			return false, nil
		}
		delete(st.tags, r.key())
		return true, nil
	})
	if cfclient.IsNotFound(err) {
		return nil
	}
	return err
}
