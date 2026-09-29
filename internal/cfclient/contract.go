// Package cfclient is the only way controllers talk to the Cloudflare API.
//
// CONTRACT (docs/plan-parallel.md §2.1). This file is frozen by the
// orchestrator: workstreams implement against it but do not edit it. Ask for
// changes in your report instead.
package cfclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client performs one Cloudflare API call. The v4 envelope is decoded:
// success=false (or a non-2xx status) yields an *APIError. Implementations are
// safe for concurrent use and share one rate limiter per token.
type Client interface {
	Do(ctx context.Context, req Request) (*Response, error)
}

// Request is relative to the base URL (e.g. "/accounts/abc/storage/kv/namespaces").
// Exactly one of Body (JSON-encoded) or RawBody (sent as-is with ContentType)
// may be set.
type Request struct {
	Method      string
	Path        string
	Query       url.Values
	Body        any
	RawBody     []byte
	ContentType string
}

// Response carries the decoded envelope of a successful call.
type Response struct {
	Status     int
	Result     json.RawMessage
	ResultInfo *ResultInfo
	Header     http.Header
}

// ResultInfo is the envelope's result_info (page- or cursor-based pagination).
type ResultInfo struct {
	Page       int    `json:"page,omitempty"`
	PerPage    int    `json:"per_page,omitempty"`
	Count      int    `json:"count,omitempty"`
	TotalCount int    `json:"total_count,omitempty"`
	TotalPages int    `json:"total_pages,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
}

// ErrorDetail is one entry of the envelope's errors array.
type ErrorDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// APIError is returned when the API answers success=false or a non-2xx status.
type APIError struct {
	Status int
	Errors []ErrorDetail
	// RetryAfter is set from the Retry-After header on 429/503, else zero.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, d := range e.Errors {
		parts = append(parts, fmt.Sprintf("%d: %s", d.Code, d.Message))
	}
	return fmt.Sprintf("cloudflare API %d [%s]", e.Status, strings.Join(parts, "; "))
}

// HasCode reports whether any error entry carries the Cloudflare error code.
func (e *APIError) HasCode(code int) bool {
	for _, d := range e.Errors {
		if d.Code == code {
			return true
		}
	}
	return false
}

// AsAPIError unwraps err to an *APIError.
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	ok := errors.As(err, &ae)
	return ae, ok
}

// IsNotFound reports an HTTP 404 from the API.
func IsNotFound(err error) bool {
	ae, ok := AsAPIError(err)
	return ok && ae.Status == http.StatusNotFound
}

// HasCode reports whether err is an *APIError carrying the given code.
func HasCode(err error, code int) bool {
	ae, ok := AsAPIError(err)
	return ok && ae.HasCode(code)
}

// Options configures a Client. Workstream A provides
//
//	func New(opts Options) (Client, error)
//
// and returns the same limiter for every Client built with the same Token.
type Options struct {
	Token      string
	BaseURL    string        // default "https://api.cloudflare.com/client/v4"; flarefake: "http://127.0.0.1:8787/client/v4"
	HTTPClient *http.Client  // optional
	RPS        float64       // per-token sustained rate; 0 → implementation default (Cloudflare global limit is 1200/5min)
	Burst      int           // 0 → implementation default
	MaxRetries int           // on 429/5xx; 0 → implementation default
	UserAgent  string        // optional
	ListTTL    time.Duration // list-cache TTL for GETs on collection paths; 0 disables
}
