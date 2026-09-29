package fake

import (
	"encoding/json"
	"net/http"
)

// APIError is one entry of the Cloudflare v4 envelope's "errors" array.
type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// envelopeStyle captures per-product quirks of the v4 envelope seen in recordings.
type envelopeStyle int

const (
	// styleDefault: result, success, errors ([]), messages ([]); result_info when given.
	styleDefault envelopeStyle = iota
	// styleNullErrorsMessages: errors and messages are null (Queues list, recording 0148).
	styleNullErrorsMessages
	// styleErrorBare: on errors, only success and errors — no result, no messages (D1, 0019).
	styleErrorBare
)

// PageInfo is the page-based result_info. Endpoints differ in which fields they return, so
// fields are pointers and omitted when nil (e.g. D1 list has no total_pages, recording 0021).
type PageInfo struct {
	Page       *int `json:"page,omitempty"`
	PerPage    *int `json:"per_page,omitempty"`
	Count      int  `json:"count"`
	TotalCount *int `json:"total_count,omitempty"`
	TotalPages *int `json:"total_pages,omitempty"`
}

func intp(i int) *int { return &i }

// response is what a profile handler returns; the server serializes it.
type response struct {
	status     int
	result     any
	resultInfo any
	errors     []APIError
	style      envelopeStyle
}

func ok(result any) response { return response{status: http.StatusOK, result: result} }

func okList(result any, info any) response {
	return response{status: http.StatusOK, result: result, resultInfo: info}
}

func fail(status, code int, msg string) response {
	return response{status: status, errors: []APIError{{Code: code, Message: msg}}}
}

func (r response) withStyle(s envelopeStyle) response { r.style = s; return r }

func writeResponse(w http.ResponseWriter, resp response) {
	if resp.status == http.StatusNoContent { // no envelope at all (tags-delete per spec; UNVERIFIED)
		w.WriteHeader(resp.status)
		return
	}
	success := len(resp.errors) == 0 && resp.status < 400
	env := map[string]any{"success": success}
	errs := any(resp.errors)
	if resp.errors == nil {
		errs = []APIError{}
	}
	switch {
	case resp.style == styleErrorBare && !success:
		env["errors"] = errs
	case resp.style == styleNullErrorsMessages && success:
		env["result"], env["errors"], env["messages"] = resp.result, nil, nil
	default:
		env["errors"], env["messages"] = errs, []any{}
		env["result"] = resp.result
		if !success {
			env["result"] = nil
		}
	}
	if resp.resultInfo != nil && success {
		env["result_info"] = resp.resultInfo
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	}
	w.WriteHeader(resp.status)
	_ = json.NewEncoder(w).Encode(env)
}

// maxPerPage bounds per_page so page arithmetic can never overflow; the spec's largest
// documented per_page maximum is 10000 (D1).
const maxPerPage = 10000

// paginate applies page/per_page to items and returns the page slice, the effective page and
// per_page (clamped, as echoed in result_info), and the total page count.
func paginate[T any](items []T, page, perPage, defPerPage int) (out []T, effPage, effPerPage, totalPages int) {
	if perPage <= 0 {
		perPage = defPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	if page <= 0 {
		page = 1
	}
	totalPages = (len(items) + perPage - 1) / perPage
	if page > totalPages+1 { // avoid (page-1)*perPage overflow; any page past the end is empty
		return items[:0], page, perPage, totalPages
	}
	start := (page - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], page, perPage, totalPages
}
