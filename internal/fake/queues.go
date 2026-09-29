package fake

import (
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// Queues: /accounts/{account_id}/queues.
// Source recordings: test/recordings/2026-09-29/0028…0035.

type queue struct {
	ID            string
	Name          string
	Created       time.Time
	Modified      time.Time
	DeliveryDelay int
	Retention     int
	Seq           int64
	// Jurisdiction is set at create only (no recording creates one). SOURCED:
	// cloudflare/workers-sdk@485cfb3:packages/wrangler/src/queues/cli/commands/create.ts#L95
	// (wrangler sends it in the create body) and the list fixture
	// packages/wrangler/src/__tests__/queues/queues.test.ts#L214 (a queue object carries it).
	Jurisdiction string
}

// queueSettings are the settings a create/update body may carry. settings.delivery_paused is
// accepted and ignored, and never returned: the recorded results omit it (0028, 0031, 0032).
// It is a real setting (SOURCED: cloudflare/workers-sdk@485cfb3:packages/wrangler/src/queues/cli/commands/pause-resume.ts#L56
// sends it in a PATCH), but whether and when the API reads it back is UNVERIFIED: wrangler's
// fixture returns it (packages/wrangler/src/__tests__/queues/queues.test.ts#L2703, a mock),
// while terraform's acceptance test for it is skipped because of "API changes causing state
// issues with delivery_paused" (cloudflare/terraform-provider-cloudflare@65783c2:internal/services/queue/resource_test.go#L75).
type queueSettings struct {
	DeliveryDelay          *int `json:"delivery_delay"`
	MessageRetentionPeriod *int `json:"message_retention_period"`
}

var queueNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const (
	queueDefaultDelay     = 0
	queueDefaultRetention = 86400 // 0028
)

func (q *queue) json() map[string]any {
	m := map[string]any{
		"queue_id": q.ID, "queue_name": q.Name,
		"settings":   map[string]any{"delivery_delay": q.DeliveryDelay, "message_retention_period": q.Retention},
		"created_on": tsMicro(q.Created), "modified_on": tsMicro(q.Modified),
	}
	if q.Jurisdiction != "" {
		// Returned only when set: 0028 has none. SOURCED (statement): wrangler's `queues list`
		// shows its jurisdiction column only when some queue carries the key, and its comment
		// expects non-jurisdictional queues to lack it,
		// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/queues/cli/commands/list.ts#L21-L36.
		// wrangler works either way (it prints "" for a missing value), so this is no dependency.
		m["jurisdiction"] = q.Jurisdiction
	}
	return m
}

// getJSON adds producer/consumer summaries, which only GET returns (0031 vs 0032).
func (q *queue) getJSON() map[string]any {
	m := q.json()
	m["producers_total_count"] = 0
	m["producers"] = []any{}
	m["consumers_total_count"] = 0
	m["consumers"] = []any{}
	return m
}

func (s *Server) registerQueues() {
	base := "/accounts/{account_id}/queues"
	s.handle(http.MethodPost, base, queueCreate)
	s.handle(http.MethodGet, base, queueList)
	s.handle(http.MethodGet, base+"/{queue_id}", queueGet)
	s.handle(http.MethodPatch, base+"/{queue_id}", queuePatch)
	s.handle(http.MethodPut, base+"/{queue_id}", queuePut)
	s.handle(http.MethodDelete, base+"/{queue_id}", queueDelete)
}

func queueValidateName(a *account, name, exceptID string) *response {
	if !queueNameRe.MatchString(name) { // 0030
		r := fail(http.StatusBadRequest, 11003, fmt.Sprintf("Queue name '%s' is invalid: invalid queue name: %s. Must match %s.", name, name, queueNameRe.String()))
		return &r
	}
	for _, q := range a.queues {
		if q.Name == name && q.ID != exceptID { // 0029: 409, unlike KV/D1 which use 400
			r := fail(http.StatusConflict, 11009, fmt.Sprintf("Queue name '%s' is already taken. Please use a different name and try again.", name))
			return &r
		}
	}
	return nil
}

func queueNotFound(id string) response {
	return fail(http.StatusNotFound, 11000, fmt.Sprintf("Queue '%s' not found. Please verify it exists and try again.", id)) // 0035
}

func queueCreate(c *reqCtx) response {
	var req struct {
		QueueName    string         `json:"queue_name"`
		Settings     *queueSettings `json:"settings"`
		Jurisdiction string         `json:"jurisdiction"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if r := queueValidateName(c.account, req.QueueName, ""); r != nil {
		return *r
	}
	if r := validateQueueSettings(req.Settings); r != nil {
		return *r
	}
	q := &queue{ID: c.s.ids.next(hex32), Name: req.QueueName, Created: c.now, Modified: c.now,
		DeliveryDelay: queueDefaultDelay, Retention: queueDefaultRetention, Seq: c.s.nextSeq(), Jurisdiction: req.Jurisdiction}
	applyQueueSettings(q, req.Settings)
	c.account.queues[q.ID] = q
	return ok(q.json())
}

// Queue settings limits: delivery delay 0 – 24 h, retention 60 s – 14 d. SOURCED:
// cloudflare/workers-sdk@485cfb3:packages/wrangler/src/queues/constants.ts#L4-L8 (the bounds
// wrangler enforces); DOCS: https://developers.cloudflare.com/queues/platform/limits/ ("up to
// 14 days", delay "24 hours"). The Free plan's 24 h retention cap is not emulated.
const (
	queueMaxDelay        = 86400
	queueMinRetention    = 60
	queueMaxRetention    = 1209600
	queueInvalidSettings = 100128 // SOURCED: …/wrangler/src/queues/constants.ts#L2 (relies: queues/utils.ts#L14 maps it to "The specified queue settings are invalid.")
)

// validateQueueSettings rejects out-of-range settings with 400/100128. The status and message
// are UNVERIFIED (not recorded).
func validateQueueSettings(st *queueSettings) *response {
	if st == nil {
		return nil
	}
	if d := st.DeliveryDelay; d != nil && (*d < 0 || *d > queueMaxDelay) {
		r := fail(http.StatusBadRequest, queueInvalidSettings, fmt.Sprintf("delivery_delay must be between 0 and %d seconds, got %d", queueMaxDelay, *d))
		return &r
	}
	if p := st.MessageRetentionPeriod; p != nil && (*p < queueMinRetention || *p > queueMaxRetention) {
		r := fail(http.StatusBadRequest, queueInvalidSettings, fmt.Sprintf("message_retention_period must be between %d and %d seconds, got %d", queueMinRetention, queueMaxRetention, *p))
		return &r
	}
	return nil
}

func applyQueueSettings(q *queue, st *queueSettings) {
	if st == nil {
		return
	}
	if st.DeliveryDelay != nil {
		q.DeliveryDelay = *st.DeliveryDelay
	}
	if st.MessageRetentionPeriod != nil {
		q.Retention = *st.MessageRetentionPeriod
	}
}

// queueList: GET …/queues[?name=…]. The name filter (repeatable, exact match) is how wrangler
// looks a queue up by name, taking the first result. SOURCED (relies):
// cloudflare/workers-sdk@485cfb3:packages/deploy-helpers/src/triggers/queue-consumers.ts#L101-L125.
// That the match is exact rather than a substring search is UNVERIFIED.
func queueList(c *reqCtx) response {
	names := c.query["name"]
	var items []*queue
	for _, q := range sortedBySeq(c.account.queues, func(q *queue) int64 { return q.Seq }) {
		if len(names) == 0 || containsStr(names, q.Name) {
			items = append(items, q)
		}
	}
	pageItems, page, perPage, totalPages := paginate(items, c.intQuery("page", 1), c.intQuery("per_page", 100), 100)
	out := make([]any, 0, len(pageItems))
	for _, q := range pageItems {
		// List items carry the producer/consumer summaries like GET (0148 recorded an empty
		// list). SOURCED (relies): wrangler's `queues list` calls producers_total_count.toString()
		// on every item, cloudflare/workers-sdk@485cfb3:packages/wrangler/src/queues/cli/commands/list.ts#L45,
		// typed as the GET shape QueueResponse (packages/deploy-helpers/src/triggers/queue-consumers.ts#L28-L39).
		out = append(out, q.getJSON())
	}
	// 0148: per_page defaults to 100, total_pages present, errors and messages are null.
	return okList(out, PageInfo{Count: len(out), Page: intp(page), PerPage: intp(perPage), TotalCount: intp(len(items)), TotalPages: intp(totalPages)}).withStyle(styleNullErrorsMessages)
}

func queueGet(c *reqCtx) response {
	q, found := c.account.queues[c.params["queue_id"]]
	if !found {
		return queueNotFound(c.params["queue_id"])
	}
	return ok(q.getJSON())
}

// queuePatch merges settings (0032).
func queuePatch(c *reqCtx) response {
	q, found := c.account.queues[c.params["queue_id"]]
	if !found {
		return queueNotFound(c.params["queue_id"])
	}
	var req struct {
		QueueName *string        `json:"queue_name"`
		Settings  *queueSettings `json:"settings"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if req.QueueName != nil {
		if r := queueValidateName(c.account, *req.QueueName, q.ID); r != nil {
			return *r
		}
		q.Name = *req.QueueName
	}
	if r := validateQueueSettings(req.Settings); r != nil {
		return *r
	}
	applyQueueSettings(q, req.Settings)
	q.Modified = c.now
	return ok(q.json())
}

// queuePut is a full replace: omitted settings are reset to defaults (0033).
func queuePut(c *reqCtx) response {
	q, found := c.account.queues[c.params["queue_id"]]
	if !found {
		return queueNotFound(c.params["queue_id"])
	}
	var req struct {
		QueueName string         `json:"queue_name"`
		Settings  *queueSettings `json:"settings"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if r := queueValidateName(c.account, req.QueueName, q.ID); r != nil {
		return *r
	}
	if r := validateQueueSettings(req.Settings); r != nil {
		return *r
	}
	q.Name = req.QueueName
	q.DeliveryDelay, q.Retention = queueDefaultDelay, queueDefaultRetention
	applyQueueSettings(q, req.Settings)
	q.Modified = c.now
	return ok(q.json())
}

func queueDelete(c *reqCtx) response {
	id := c.params["queue_id"]
	if _, found := c.account.queues[id]; !found {
		return queueNotFound(id)
	}
	delete(c.account.queues, id)
	return ok(nil) // 0034
}
