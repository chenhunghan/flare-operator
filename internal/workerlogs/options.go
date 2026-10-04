package workerlogs

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ParseOptions reads the kubelet containerLogs query (follow, tailLines, sinceSeconds, sinceTime,
// timestamps, limitBytes, previous) with the kubelet's rules, telling an absent tailLines (nil)
// from tailLines=0. sinceSeconds becomes SinceTime = now - s.
//
// The parsing follows the query-parameter conversions the kubelet applies to v1.PodLogOptions
// (k8s.io/apimachinery@v0.37.1 pkg/runtime/conversion.go Convert_Slice_string_To_bool and
// Convert_Slice_string_To_Pointer_int64; pkg/apis/meta/v1/time.go UnmarshalQueryParameter), and
// the validation is that of ValidatePodLogOptions (tailLines ≥ 0, limitBytes ≥ 1,
// sinceSeconds ≥ 1, at most one of sinceSeconds and sinceTime). Unknown parameters (container,
// stream, insecureSkipTLSVerifyBackend, …) are ignored.
func ParseOptions(q url.Values, now time.Time) (Options, error) {
	var o Options
	o.Follow = queryBool(q, "follow")
	o.Previous = queryBool(q, "previous")
	o.Timestamps = queryBool(q, "timestamps")

	var err error
	if o.TailLines, err = queryInt64(q, "tailLines"); err != nil {
		return Options{}, err
	}
	if o.LimitBytes, err = queryInt64(q, "limitBytes"); err != nil {
		return Options{}, err
	}
	sinceSeconds, err := queryInt64(q, "sinceSeconds")
	if err != nil {
		return Options{}, err
	}
	var sinceTime *time.Time
	if vs, ok := q["sinceTime"]; ok {
		s := ""
		if len(vs) > 0 {
			s = vs[0]
		}
		t := time.Time{}
		// An empty value or "null" is the zero time, as in metav1.Time.UnmarshalQueryParameter.
		if s != "" && s != "null" {
			if t, err = time.Parse(time.RFC3339, s); err != nil {
				return Options{}, fmt.Errorf("invalid sinceTime %q: %w", s, err)
			}
		}
		sinceTime = &t
	}

	if o.TailLines != nil && *o.TailLines < 0 {
		return Options{}, fmt.Errorf("tailLines: Invalid value: %d: must be greater than or equal to 0", *o.TailLines)
	}
	if o.LimitBytes != nil && *o.LimitBytes < 1 {
		return Options{}, fmt.Errorf("limitBytes: Invalid value: %d: must be greater than 0", *o.LimitBytes)
	}
	switch {
	case sinceSeconds != nil && sinceTime != nil:
		return Options{}, fmt.Errorf("at most one of `sinceTime` or `sinceSeconds` may be specified")
	case sinceSeconds != nil:
		if *sinceSeconds < 1 {
			return Options{}, fmt.Errorf("sinceSeconds: Invalid value: %d: must be greater than 0", *sinceSeconds)
		}
		// Clamp absurd values instead of overflowing time.Duration; the query window is clamped
		// to maxQueryWindow anyway.
		secs := min(*sinceSeconds, int64(math.MaxInt64/int64(time.Second)))
		t := now.Add(-time.Duration(secs) * time.Second)
		o.SinceTime = &t
	case sinceTime != nil && !sinceTime.IsZero():
		// A zero sinceTime means "no lower bound": the default window applies.
		o.SinceTime = sinceTime
	}
	return o, nil
}

// queryBool is Convert_Slice_string_To_bool: absent, "0" and "false" (any case) are false;
// anything else, the empty string included, is true.
func queryBool(q url.Values, key string) bool {
	vs, ok := q[key]
	if !ok || len(vs) == 0 {
		return false
	}
	return vs[0] != "0" && !strings.EqualFold(vs[0], "false")
}

// queryInt64 is Convert_Slice_string_To_Pointer_int64: absent is nil; otherwise the first value
// must be a base-10 int64.
func queryInt64(q url.Values, key string) (*int64, error) {
	vs, ok := q[key]
	if !ok || len(vs) == 0 {
		return nil, nil
	}
	n, err := strconv.ParseInt(vs[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", key, vs[0], err)
	}
	return &n, nil
}

// maxQueryWindow bounds timeframe.from: a 30-day window is accepted by telemetry/query
// (recording 0098); a longer one is UNVERIFIED, and no plan retains logs that long (3 days Free,
// 7 days Paid; DOCS: https://developers.cloudflare.com/workers/observability/logs/workers-logs/).
const maxQueryWindow = 30 * 24 * time.Hour

// withDefaults fills zero Limits fields with the package defaults and clamps PageSize.
func (l Limits) withDefaults() Limits {
	if l.DefaultWindow <= 0 {
		l.DefaultWindow = DefaultWindow
	}
	if l.MaxEvents <= 0 {
		l.MaxEvents = DefaultMaxEvents
	}
	if l.PageSize <= 0 || l.PageSize > MaxPageSize {
		l.PageSize = MaxPageSize
	}
	if l.TailPingInterval <= 0 {
		l.TailPingInterval = DefaultTailPingInterval
	}
	if l.MaxFollowers <= 0 {
		l.MaxFollowers = DefaultMaxFollowers
	}
	return l
}
