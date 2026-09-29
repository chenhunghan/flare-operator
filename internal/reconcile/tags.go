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
// PUT replaces the whole tag map, so every change is GET → merge → PUT. Reading the tags of a
// never-tagged resource answers HTTP 500 (docs/cloudflare-service-catalog.md); a 500 is
// therefore treated as "no tags" after one retry. That is also what a transient 500 would
// look like, so a following PUT could drop foreign tags in that (rare) case; the contract's
// Request has no header field, so the PUT cannot carry If-Match to guard against it
// (requested as a contract change).
type ResourceTagger struct {
	// Key overrides OwnerTagKey (tests).
	Key string
}

func (r ResourceTagger) key() string {
	if r.Key != "" {
		return r.Key
	}
	return OwnerTagKey
}

type tagsResult struct {
	Tags map[string]string `json:"tags"`
	ETag string            `json:"etag"`
}

// Get returns the resource's tags; tagged=false when the resource was never tagged (500).
func (ResourceTagger) Get(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget) (tags map[string]string, tagged bool, err error) {
	q := url.Values{"resource_type": {t.Type}, "resource_id": {t.ID}}
	if t.WorkerID != "" {
		q.Set("worker_id", t.WorkerID)
	}
	ctx = cfclient.WithMaxRetries(cfclient.WithoutCache(ctx), 1)
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: tagsPath(accountID), Query: q})
	if err != nil {
		if ae, ok := cfclient.AsAPIError(err); ok && ae.Status == http.StatusInternalServerError {
			return map[string]string{}, false, nil
		}
		return nil, false, err
	}
	var res tagsResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, false, fmt.Errorf("decode tags of %s %s: %w", t.Type, t.ID, err)
	}
	if res.Tags == nil {
		res.Tags = map[string]string{}
	}
	return res.Tags, true, nil
}

func tagsPath(accountID string) string { return "/accounts/" + accountID + "/tags" }

func (r ResourceTagger) put(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget, tags map[string]string) error {
	body := map[string]any{"resource_type": t.Type, "resource_id": t.ID, "tags": tags}
	if t.WorkerID != "" {
		body["worker_id"] = t.WorkerID
	}
	_, err := cf.Do(ctx, cfclient.Request{Method: http.MethodPut, Path: tagsPath(accountID), Body: body})
	return err
}

// EnsureOwner implements Tagger.
func (r ResourceTagger) EnsureOwner(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget, owner string) error {
	tags, _, err := r.Get(ctx, cf, accountID, t)
	if err != nil {
		return err
	}
	cur, has := tags[r.key()]
	switch {
	case has && cur == owner:
		return nil
	case has && cur != "":
		return &OwnershipConflictError{Target: t, Owner: cur, Wanted: owner}
	}
	tags[r.key()] = owner
	return r.put(ctx, cf, accountID, t, tags)
}

// RemoveOwner implements Tagger.
func (r ResourceTagger) RemoveOwner(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget, owner string) error {
	tags, tagged, err := r.Get(ctx, cf, accountID, t)
	if err != nil {
		if cfclient.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !tagged || tags[r.key()] != owner {
		return nil
	}
	delete(tags, r.key())
	if len(tags) == 0 {
		body := map[string]any{"resource_type": t.Type, "resource_id": t.ID}
		if t.WorkerID != "" {
			body["worker_id"] = t.WorkerID
		}
		_, err := cf.Do(ctx, cfclient.Request{Method: http.MethodDelete, Path: tagsPath(accountID), Body: body})
		return err
	}
	return r.put(ctx, cf, accountID, t, tags)
}
