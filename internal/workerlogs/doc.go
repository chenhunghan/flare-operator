// Package workerlogs reads a Cloudflare Worker's logs and renders them as the line stream that
// `kubectl logs` expects. It is the log source of the Workers virtual kubelet (internal/vk);
// the design, with its evidence, is docs/workers-logs-design.md.
//
// Frozen contract (contract.go); change deliberately. Workstream A implements it:
//
//   - Query (non-follow): POST /accounts/{account_id}/workers/observability/telemetry/query
//     with view "events", dry true, the filter $metadata.service eq <script> and a timeframe;
//     limit at most MaxPageSize (a larger limit is a 400 too_big, recording 0096). Events come
//     newest first (recording 0054); further pages pass offset=<last $metadata.id> with
//     offsetDirection "next" (recording 0093). Query returns them oldest first.
//   - Follow: POST /accounts/{account_id}/workers/scripts/{script}/tails (body {}, recording
//     0064) returns {id, url, expires_at}; url is a wss:// capability URL (anyone holding it
//     reads the logs) dialled with subprotocol trace-v1. The client sends {"debug":false} once
//     open and pings every 10 s (SOURCED: cloudflare/workers-sdk@wrangler@4.143.0:
//     packages/wrangler/src/tail/createTail.ts and src/tail/index.ts, read in the published
//     wrangler-dist/cli.js). Frames are JSON trace events (outcome, eventTimestamp, event,
//     logs[{level, message[], timestamp}], exceptions[{name, message, stack, timestamp}]; SOURCED:
//     the same release, src/tail/printing.ts). The session ends with a close of our socket and
//     DELETE …/tails/{id} (recording 0074): a DELETE alone does not disconnect a client
//     (docs/spike-results-2026-09-29.md §1). One upstream tail per (account, script) is shared by
//     every concurrent follower in the process (Cloudflare allows 10 viewers per Worker; DOCS:
//     https://developers.cloudflare.com/workers/observability/logs/real-time-logs/).
//   - The tail URL never leaves this package: it is not logged, not put into errors, events,
//     metrics or status, and never returned to a kubelet API caller. Errors that could carry it
//     (URL dial errors) are wrapped so that only the scheme and host remain.
//
// Ordering: Cloudflare freezes timestamp for the whole invocation (one value per request), so
// events with equal Time are ordered by Seq ($metadata.id, whose suffix counts up within the
// invocation; recording 0054). Per invocation the formatter writes its console lines, then its
// exceptions, then the invocation summary, for both sources.
package workerlogs
