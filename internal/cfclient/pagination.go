package cfclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// MaxListPages guards ListAll against endpoints that never signal the last page.
const MaxListPages = 1000

// ListAll GETs req.Path repeatedly and concatenates the result arrays of every page.
//
// Both pagination styles of the v4 API are handled, detected per response:
//   - cursor-based: result_info.cursor is non-empty → the next request sets ?cursor=…;
//     an empty cursor ends the listing.
//   - page-based: ?page=N is incremented until page ≥ result_info.total_pages, or — when the
//     endpoint omits total_pages (D1, recording 0021) — until a page returns fewer than
//     per_page items (or none).
//
// A response without result_info is a single, complete page. Callers may preset per_page (or
// other filters) in req.Query; ListAll never mutates the caller's Query.
func ListAll(ctx context.Context, c Client, req Request) ([]json.RawMessage, error) {
	req.Method = http.MethodGet
	q := url.Values{}
	for k, v := range req.Query {
		q[k] = append([]string(nil), v...)
	}
	page := 1
	if p := q.Get("page"); p != "" {
		if _, err := fmt.Sscan(p, &page); err != nil || page < 1 {
			page = 1
		}
	}
	var all []json.RawMessage
	for i := 0; i < MaxListPages; i++ {
		req.Query = q
		resp, err := c.Do(ctx, req)
		if err != nil {
			return nil, err
		}
		var items []json.RawMessage
		if len(resp.Result) > 0 && string(resp.Result) != "null" {
			if err := json.Unmarshal(resp.Result, &items); err != nil {
				return nil, fmt.Errorf("cfclient: ListAll %s: result is not an array: %w", req.Path, err)
			}
		}
		all = append(all, items...)
		ri := resp.ResultInfo
		switch {
		case ri == nil:
			return all, nil
		case ri.Cursor != "":
			if q.Get("cursor") == ri.Cursor || len(items) == 0 {
				return all, nil // defensive: a repeated cursor would loop forever
			}
			q = cloneValues(q)
			q.Set("cursor", ri.Cursor)
			q.Del("page")
		case q.Get("cursor") != "":
			return all, nil // cursor listing ended with an empty cursor
		default:
			if len(items) == 0 {
				return all, nil
			}
			if ri.TotalPages > 0 {
				if page >= ri.TotalPages {
					return all, nil
				}
			} else {
				perPage := ri.PerPage
				if perPage == 0 {
					if v := q.Get("per_page"); v != "" {
						_, _ = fmt.Sscan(v, &perPage)
					}
				}
				if perPage == 0 || len(items) < perPage {
					return all, nil
				}
				if ri.TotalCount > 0 && len(all) >= ri.TotalCount {
					return all, nil
				}
			}
			page++
			q = cloneValues(q)
			q.Set("page", fmt.Sprint(page))
		}
	}
	return nil, fmt.Errorf("cfclient: ListAll %s: more than %d pages", req.Path, MaxListPages)
}

// ListAllInto is ListAll decoding each item into T.
func ListAllInto[T any](ctx context.Context, c Client, req Request) ([]T, error) {
	raw, err := ListAll(ctx, c, req)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(raw))
	for _, r := range raw {
		var v T
		if err := json.Unmarshal(r, &v); err != nil {
			return nil, fmt.Errorf("cfclient: ListAllInto %s: %w", req.Path, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func cloneValues(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}
