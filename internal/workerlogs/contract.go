package workerlogs

import (
	"context"
	"errors"
	"io"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
)

// Workstream A provides, besides the implementations of the interfaces below:
//
//	// ParseOptions reads the kubelet containerLogs query (follow, tailLines, sinceSeconds,
//	// sinceTime, timestamps, limitBytes, previous) with the kubelet's validation rules, telling
//	// an absent tailLines (nil) from tailLines=0. sinceSeconds becomes SinceTime = now - s.
//	func ParseOptions(q url.Values, now time.Time) (Options, error)
//
//	// NewSource returns the Cloudflare-backed Source (telemetry query + legacy tail).
//	func NewSource(l Limits, opts ...SourceOption) Source
//
//	// NewFormatter returns the line format of docs/workers-logs-design.md §7.
//	func NewFormatter() Formatter
//
//	// NewStreamer joins a Source and a Formatter into the kubelet log stream.
//	func NewStreamer(src Source, f Formatter, l Limits, now func() time.Time) Streamer

// Constants of the Cloudflare APIs used here.
const (
	// MaxPageSize is the largest telemetry/query limit Cloudflare accepts (2000; 5000 is a 400
	// too_big, recording 0096).
	MaxPageSize = 2000
	// QueryID is the queryId sent with every ad-hoc telemetry query (dry: true, so nothing is
	// saved; recording 0054 used an ad-hoc ID the same way).
	QueryID = "flare-operator-logs"
	// TailSubprotocol is the WebSocket subprotocol of the legacy tail (spike §1; wrangler).
	TailSubprotocol = "trace-v1"
	// TimestampFormat prefixes each line when Options.Timestamps is set: the kubelet's fixed-width
	// RFC 3339 with nanoseconds, always in UTC.
	TimestampFormat = "2006-01-02T15:04:05.000000000Z07:00"
)

// Defaults of Limits (the virtual kubelet's flags override them).
const (
	// DefaultWindow is how far back a query without SinceTime reaches: the Free plan's 3-day
	// retention (DOCS: https://developers.cloudflare.com/workers/observability/logs/workers-logs/).
	DefaultWindow = 72 * time.Hour
	// DefaultMaxEvents bounds one non-follow request or the history of a follow (5 pages).
	DefaultMaxEvents = 10000
	// DefaultTailPingInterval is wrangler's keep-alive ping interval (PING_INTERVAL_MS 1e4).
	DefaultTailPingInterval = 10 * time.Second
	// DefaultMaxFollowers bounds concurrent follow sessions per process.
	DefaultMaxFollowers = 100
)

// Target is one Worker whose logs are read, with the credentials to read them. The virtual
// kubelet builds it from a stand-in Pod (internal/vk PodResolver).
type Target struct {
	// Client is the CloudflareAccount's API client (rate-limited per token).
	Client cfclient.Client
	// AccountID is the Cloudflare account ID.
	AccountID string
	// Script is the Cloudflare script name ($metadata.service of its events).
	Script string
	// WorkerScript names the object, for log messages and metrics only (never sent to
	// Cloudflare).
	WorkerScript types.NamespacedName
	// AllowInsecureTail permits a ws:// tail URL. Only true when the account uses an allowed
	// spec.baseURL override (flarefake); otherwise a tail URL that is not wss:// is refused.
	AllowInsecureTail bool
}

// Options are the kubelet log options of one request (corev1.PodLogOptions minus container).
type Options struct {
	// Follow streams new events until the request is cancelled (kubectl logs -f).
	Follow bool
	// TailLines keeps only the last N lines of the history (nil: all, up to Limits.MaxEvents
	// events; 0: none, so a follow shows only new events).
	TailLines *int64
	// SinceTime drops events before it (nil: now - Limits.DefaultWindow). It is the query's
	// timeframe.from.
	SinceTime *time.Time
	// Timestamps prefixes every line with its time in TimestampFormat and a space.
	Timestamps bool
	// LimitBytes ends the stream after this many bytes (nil: no limit).
	LimitBytes *int64
	// Previous asks for a previous container instance's logs; unsupported
	// (ErrPreviousUnsupported).
	Previous bool
}

// Limits bound the work and the Cloudflare API calls of one request.
type Limits struct {
	// DefaultWindow is the history window when Options.SinceTime is nil (default DefaultWindow).
	DefaultWindow time.Duration
	// MaxEvents is the most events one history read returns (default DefaultMaxEvents); each
	// MaxPageSize events cost one API request.
	MaxEvents int
	// PageSize is the telemetry query limit per request, at most MaxPageSize (default
	// MaxPageSize).
	PageSize int
	// TailPingInterval is the WebSocket keep-alive ping interval of a follow (default
	// DefaultTailPingInterval); a missing pong by the next ping reconnects.
	TailPingInterval time.Duration
	// MaxFollowers bounds concurrent follow sessions in the process (default
	// DefaultMaxFollowers); beyond it Logs returns ErrTooManyFollowers.
	MaxFollowers int
}

// EventKind classifies an Event.
type EventKind string

// Event kinds.
const (
	// KindLog is one console.* call ($metadata.type cf-worker; a tail frame's logs[]).
	KindLog EventKind = "log"
	// KindException is an uncaught exception (an event with source.exception; a tail frame's
	// exceptions[]).
	KindException EventKind = "exception"
	// KindInvocation is the summary of one invocation ($metadata.type cf-worker-event; the event
	// and outcome of a tail frame).
	KindInvocation EventKind = "invocation"
	// KindNotice is a message about the stream itself: Cloudflare's tail sampling notices
	// (tail-info overload / overload-stop) and the follower's own reconnect gap notice.
	KindNotice EventKind = "notice"
)

// Event is one log record from either source, normalized.
type Event struct {
	// Time is Cloudflare's timestamp (ms precision; constant within one invocation).
	Time time.Time
	// Seq orders events with equal Time: $metadata.id for Query; for Follow, a counter that
	// keeps the order of the frame (logs, then exceptions, then the invocation).
	Seq  string
	Kind EventKind
	// Level of a KindLog event: log, debug, info, warn or error ("" when Cloudflare gives none).
	Level string
	// Message of a KindLog or KindNotice event. Console arguments are joined with single spaces;
	// a non-string argument is written as compact JSON.
	Message string
	// Exception is set for KindException.
	Exception *Exception
	// Invocation is set for KindInvocation.
	Invocation *Invocation
	// ScriptVersion is $workers.scriptVersion.id (the frame's scriptVersion.id for Follow).
	ScriptVersion string
	// RequestID is $metadata.requestId (empty for Follow).
	RequestID string
	// Truncated is $workers.truncated: Cloudflare cut the record (over 256 KB).
	Truncated bool
}

// Exception is an uncaught exception.
type Exception struct {
	Name    string
	Message string
	// Stack is the JavaScript stack, newline-separated ("" when absent).
	Stack string
}

// Invocation summarizes one invocation of the Worker.
type Invocation struct {
	// Type is the trigger: fetch, scheduled, queue, alarm, email, rpc, tail, or unknown.
	Type string
	// Outcome is Cloudflare's outcome: ok, exception, exceededCpu, exceededMemory, canceled,
	// unknown ("" when not reported).
	Outcome string
	// Method and URL of a fetch.
	Method string
	URL    string
	// Status is the HTTP response status of a fetch (0: unknown).
	Status int
	// Cron of a scheduled event.
	Cron string
	// Queue and BatchSize of a queue event.
	Queue     string
	BatchSize int
	// MailFrom, RcptTo and RawSize of an email event.
	MailFrom string
	RcptTo   string
	RawSize  int64
	// Entrypoint and RPCMethod of an RPC call.
	Entrypoint string
	RPCMethod  string
	// WallTimeMs and CPUTimeMs ($workers.wallTimeMs, cpuTimeMs; nil when not reported).
	WallTimeMs *float64
	CPUTimeMs  *float64
	// RayID is $metadata.rayId (empty for Follow).
	RayID string
}

// Line is one output line before the optional timestamp prefix. Text has no newline.
type Line struct {
	Time time.Time
	Text string
}

// Formatter renders events as lines (docs/workers-logs-design.md §7). An exception's stack
// becomes one line per stack frame, all with the event's Time. Format is pure and safe for
// concurrent use.
type Formatter interface {
	Format(ev Event) []Line
}

// QueryOptions select the events of one history read.
type QueryOptions struct {
	// From and To bound the events' Time (the query timeframe; From inclusive).
	From time.Time
	To   time.Time
	// MaxEvents is the most events returned: the newest ones of the window. 0 means
	// Limits.MaxEvents.
	MaxEvents int
}

// QueryResult is the outcome of a history read.
type QueryResult struct {
	// Events, oldest first.
	Events []Event
	// More reports that the window holds older events than those returned (MaxEvents reached).
	More bool
}

// Source reads a Worker's events from Cloudflare.
type Source interface {
	// Query returns the newest events of the window, oldest first (telemetry/query, paged).
	Query(ctx context.Context, t Target, q QueryOptions) (QueryResult, error)
	// Follow subscribes to live events (legacy tail). The upstream tail of (t.AccountID,
	// t.Script) is shared by concurrent followers; the last Close of a subscription closes the
	// socket and deletes the tail. Follow reconnects (new tail, wrangler's 1, 2, 4, 8, 16 s
	// back-off) on an abnormal close, a missed pong or the tail's expires_at, and emits a
	// KindNotice for the gap. It fails only when the tail cannot be created at all.
	Follow(ctx context.Context, t Target) (Stream, error)
}

// Stream is one subscription to live events.
type Stream interface {
	// Events delivers events in arrival order; it is closed when the stream ends.
	Events() <-chan Event
	// Err is why Events was closed (nil after Close or a cancelled context).
	Err() error
	// Close ends the subscription; safe to call more than once.
	Close() error
}

// Streamer produces the body of a kubelet containerLogs response.
type Streamer interface {
	// Logs returns the formatted stream: the history (Source.Query, honouring TailLines,
	// SinceTime and Limits), then, with Follow, live events (Source.Follow, opened before the
	// history is read) until ctx ends. The reader applies Timestamps and LimitBytes. Closing it
	// ends the follow.
	Logs(ctx context.Context, t Target, opts Options) (io.ReadCloser, error)
}

// Errors returned by Streamer.Logs (the virtual kubelet maps them to HTTP statuses).
var (
	// ErrPreviousUnsupported: --previous has no meaning for a Worker (400).
	ErrPreviousUnsupported = errors.New("previous logs are not available for Cloudflare Workers stand-in pods")
	// ErrTooManyFollowers: Limits.MaxFollowers follow sessions are open (429).
	ErrTooManyFollowers = errors.New("too many concurrent `kubectl logs -f` sessions on this virtual kubelet")
	// ErrInsecureTailURL: Cloudflare returned a tail URL that is not wss:// and the target does
	// not allow it (502).
	ErrInsecureTailURL = errors.New("the tail URL returned by the API is not wss://")
)
