package vk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/workerlogs"
)

// ParseOptionsFunc reads the kubelet containerLogs query into workerlogs.Options
// (workerlogs.ParseOptions has this signature).
type ParseOptionsFunc func(q url.Values, now time.Time) (workerlogs.Options, error)

// errInvalidQuery marks a query the kubelet would refuse (400).
var errInvalidQuery = errors.New("invalid log options")

// publicError is what a kubelet API caller sees for err: a status and a fixed message. The
// details (account names, Cloudflare error codes, internal errors) are logged, never returned.
// The 400 for an invalid query keeps its detail: it only echoes the caller's own parameters.
func publicError(err error) (int, string) {
	var ae *reconcile.AccountError
	var api *cfclient.APIError
	switch {
	case errors.Is(err, errInvalidQuery):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, workerlogs.ErrPreviousUnsupported):
		return http.StatusBadRequest, workerlogs.ErrPreviousUnsupported.Error()
	case errors.Is(err, ErrContainerNotFound):
		return http.StatusNotFound, ErrContainerNotFound.Error()
	case errors.Is(err, ErrNotStandIn):
		return http.StatusNotFound, ErrNotStandIn.Error()
	case errors.Is(err, ErrPodNotFound):
		return http.StatusNotFound, ErrPodNotFound.Error()
	case errors.Is(err, ErrUnsupported):
		return http.StatusNotImplemented, ErrUnsupported.Error()
	case errors.Is(err, workerlogs.ErrTooManyFollowers):
		return http.StatusTooManyRequests, workerlogs.ErrTooManyFollowers.Error()
	case errors.Is(err, errTooManyRequests), errors.Is(err, errTooManyFollowersScript), errors.Is(err, errTooManyFollowersNamespace):
		return http.StatusTooManyRequests, err.Error()
	case errors.Is(err, workerlogs.ErrInsecureTailURL):
		return http.StatusBadGateway, "upstream error: the Cloudflare tail could not be opened securely"
	case errors.As(err, &ae):
		return http.StatusServiceUnavailable, "the WorkerScript's Cloudflare account is not ready (see the CloudflareAccount's Ready condition)"
	case errors.As(err, &api):
		switch api.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			// Forbidden, not Unauthorized (which would read as the caller's own authentication
			// failing).
			return http.StatusForbidden, "the Cloudflare API refused the account's token (it needs Workers Observability and Workers Tail permissions)"
		case http.StatusNotFound:
			return http.StatusNotFound, "the Worker was not found in Cloudflare"
		case http.StatusTooManyRequests:
			return http.StatusTooManyRequests, "the Cloudflare API rate limit was reached; retry later"
		}
		return http.StatusBadGateway, "upstream error from the Cloudflare API"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "upstream timeout"
	}
	return http.StatusInternalServerError, "internal error"
}

// statusOf is publicError's status.
func statusOf(err error) int {
	s, _ := publicError(err)
	return s
}

// writeError answers err with its public status and message.
func writeError(w http.ResponseWriter, err error) {
	s, msg := publicError(err)
	http.Error(w, msg, s)
}

// logsHandler serves GET /containerLogs/{namespace}/{pod}/{container}.
type logsHandler struct {
	resolver PodResolver
	streamer workerlogs.Streamer
	parse    ParseOptionsFunc
	now      func() time.Time
	log      logr.Logger
	limits   ServerLimits
	queries  chan struct{} // one per running non-follow request
	follows  *followLimiter
}

func newLogsHandler(c HandlerConfig) *logsHandler {
	l := c.Limits.WithDefaults()
	return &logsHandler{
		resolver: c.Resolver, streamer: c.Streamer, parse: c.ParseOptions, now: c.Now, log: c.Log.WithName("logs"),
		limits: l, queries: make(chan struct{}, l.MaxConcurrentRequests), follows: newFollowLimiter(l),
	}
}

func (h *logsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ns, pod, container := r.PathValue("namespace"), r.PathValue("pod"), r.PathValue("container")
	log := h.log.WithValues("pod", ns+"/"+pod, "container", container)
	fail := func(what string, err error) {
		if s := statusOf(err); s >= 500 || s == http.StatusForbidden {
			log.Info(what, "error", err.Error())
		} else {
			log.V(1).Info(what, "error", err.Error())
		}
		writeError(w, err)
	}
	opts, err := h.parse(r.URL.Query(), h.now())
	if err != nil {
		if !errors.Is(err, errInvalidQuery) {
			err = fmt.Errorf("%w: %s", errInvalidQuery, err.Error())
		}
		fail("parse log options", err)
		return
	}
	if opts.Previous {
		fail("log options", workerlogs.ErrPreviousUnsupported)
		return
	}
	if !opts.Follow {
		select {
		case h.queries <- struct{}{}:
			defer func() { <-h.queries }()
		default:
			fail("log request", errTooManyRequests)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.limits.MaxStreamDuration)
	defer cancel()
	target, err := h.resolver.Resolve(ctx, ns, pod, container)
	if err != nil {
		fail("resolve stand-in pod", err)
		return
	}
	if opts.Follow {
		release, err := h.follows.acquire(target.AccountID+"/"+target.Script, ns)
		if err != nil {
			fail("follow", err)
			return
		}
		defer release()
	}
	rc, err := h.streamer.Logs(ctx, target, opts)
	if err != nil {
		if ctx.Err() == nil {
			fail("read worker logs", err)
		}
		return
	}
	// Closing the reader ends a follow; do it as soon as the caller goes away or the stream's
	// time is up, even while a Read is blocked.
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	defer func() {
		stop()
		_ = rc.Close()
	}()
	var body io.Reader = rc
	if opts.LimitBytes != nil {
		body = io.LimitReader(rc, *opts.LimitBytes)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fw := newFlushWriter(w, h.limits.WriteTimeout, time.Now)
	w.WriteHeader(http.StatusOK)
	if err := fw.flush(); err != nil {
		return
	}
	if _, err := io.Copy(fw, body); err != nil && ctx.Err() == nil && !isClosed(err) {
		log.Info("stream worker logs", "workerScript", target.WorkerScript, "error", err.Error())
	}
}

func isClosed(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "closed")
}

// flushWriter flushes after every write, so a follow delivers each line when it arrives, and
// gives every write (and flush) a deadline of timeout: a caller that stops reading cannot hold
// a stream (and its follow slot) longer than that.
type flushWriter struct {
	w       io.Writer
	rc      *http.ResponseController
	timeout time.Duration
	now     func() time.Time
}

func newFlushWriter(w http.ResponseWriter, timeout time.Duration, now func() time.Time) *flushWriter {
	return &flushWriter{w: w, rc: http.NewResponseController(w), timeout: timeout, now: now}
}

func (fw *flushWriter) deadline() {
	// ErrNotSupported (a test recorder) leaves writes without a deadline.
	_ = fw.rc.SetWriteDeadline(fw.now().Add(fw.timeout))
}

func (fw *flushWriter) flush() error {
	fw.deadline()
	if err := fw.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	fw.deadline()
	n, err := fw.w.Write(p)
	if err != nil {
		return n, err
	}
	return n, fw.flush()
}
