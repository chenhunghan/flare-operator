package workerlogs

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseOptions(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	since := time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)
	cases := []struct {
		query   string
		want    Options
		wantErr string
	}{
		{query: "", want: Options{}},
		{query: "follow=true&timestamps=1&previous=false", want: Options{Follow: true, Timestamps: true}},
		// Convert_Slice_string_To_bool: only absent, "0" and "false" (any case) are false.
		{query: "follow=", want: Options{Follow: true}},
		{query: "follow=FALSE&timestamps=0", want: Options{}},
		{query: "follow=yes&previous=1", want: Options{Follow: true, Previous: true}},
		// Absent tailLines is nil; tailLines=0 is a pointer to 0.
		{query: "tailLines=0", want: Options{TailLines: i64(0)}},
		{query: "tailLines=50", want: Options{TailLines: i64(50)}},
		{query: "tailLines=-1", wantErr: "tailLines"},
		{query: "tailLines=abc", wantErr: "tailLines"},
		{query: "tailLines=", wantErr: "tailLines"},
		{query: "limitBytes=10", want: Options{LimitBytes: i64(10)}},
		{query: "limitBytes=0", wantErr: "limitBytes"},
		{query: "sinceSeconds=600", want: Options{SinceTime: ptrT(now.Add(-10 * time.Minute))}},
		{query: "sinceSeconds=0", wantErr: "sinceSeconds"},
		{query: "sinceSeconds=9223372036854775807", want: Options{SinceTime: ptrT(now.Add(-time.Duration(9223372036) * time.Second))}},
		{query: "sinceTime=2026-10-04T11:00:00Z", want: Options{SinceTime: &since}},
		{query: "sinceTime=2026-10-04T13:00:00%2B02:00", want: Options{SinceTime: &since}},
		{query: "sinceTime=yesterday", wantErr: "sinceTime"},
		{query: "sinceTime=", want: Options{}},
		{query: "sinceTime=2026-10-04T11:00:00Z&sinceSeconds=5", wantErr: "at most one"},
		{query: "container=worker&insecureSkipTLSVerifyBackend=true", want: Options{}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseOptions(q, now)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !optsEqual(got, tc.want) {
				t.Errorf("got %s, want %s", fmtOpts(got), fmtOpts(tc.want))
			}
		})
	}
}

func ptrT(t time.Time) *time.Time { return &t }

func optsEqual(a, b Options) bool {
	eqI := func(x, y *int64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	eqT := func(x, y *time.Time) bool { return (x == nil) == (y == nil) && (x == nil || x.Equal(*y)) }
	return a.Follow == b.Follow && a.Previous == b.Previous && a.Timestamps == b.Timestamps &&
		eqI(a.TailLines, b.TailLines) && eqI(a.LimitBytes, b.LimitBytes) && eqT(a.SinceTime, b.SinceTime)
}

func fmtOpts(o Options) string {
	var b strings.Builder
	b.WriteString("{")
	if o.Follow {
		b.WriteString(" follow")
	}
	if o.Previous {
		b.WriteString(" previous")
	}
	if o.Timestamps {
		b.WriteString(" timestamps")
	}
	if o.TailLines != nil {
		b.WriteString(" tail=" + itoa(*o.TailLines))
	}
	if o.LimitBytes != nil {
		b.WriteString(" limit=" + itoa(*o.LimitBytes))
	}
	if o.SinceTime != nil {
		b.WriteString(" since=" + o.SinceTime.UTC().Format(time.RFC3339))
	}
	b.WriteString(" }")
	return b.String()
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestLimitsDefaults(t *testing.T) {
	l := Limits{PageSize: 5000}.withDefaults()
	if l.PageSize != MaxPageSize || l.MaxEvents != DefaultMaxEvents || l.DefaultWindow != DefaultWindow ||
		l.TailPingInterval != DefaultTailPingInterval || l.MaxFollowers != DefaultMaxFollowers {
		t.Errorf("defaults = %+v", l)
	}
	if l := (Limits{PageSize: 100, MaxEvents: 7}).withDefaults(); l.PageSize != 100 || l.MaxEvents != 7 {
		t.Errorf("explicit values changed: %+v", l)
	}
}
