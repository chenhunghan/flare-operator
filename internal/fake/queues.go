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
	// Jurisdiction is set at create only. UNVERIFIED: no recording creates a queue with a
	// jurisdiction; the spec lists it in the create body and in the queue object.
	Jurisdiction string
}

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
		m["jurisdiction"] = q.Jurisdiction // UNVERIFIED: returned only when set (0028 has none)
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

// validateQueueSettings enforces documented Queues limits (delivery delay ≤ 12 h, retention
// 60 s – 14 d). Codes/messages UNVERIFIED (not yet recorded).
func validateQueueSettings(st *queueSettings) *response {
	if st == nil {
		return nil
	}
	if d := st.DeliveryDelay; d != nil && (*d < 0 || *d > 43200) {
		r := fail(http.StatusBadRequest, 11003, fmt.Sprintf("delivery_delay must be between 0 and 43200 seconds, got %d", *d))
		return &r
	}
	if p := st.MessageRetentionPeriod; p != nil && (*p < 60 || *p > 1209600) {
		r := fail(http.StatusBadRequest, 11003, fmt.Sprintf("message_retention_period must be between 60 and 1209600 seconds, got %d", *p))
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

func queueList(c *reqCtx) response {
	items := sortedBySeq(c.account.queues, func(q *queue) int64 { return q.Seq })
	pageItems, page, perPage, totalPages := paginate(items, c.intQuery("page", 1), c.intQuery("per_page", 100), 100)
	out := make([]any, 0, len(pageItems))
	for _, q := range pageItems {
		out = append(out, q.json()) // item shape UNVERIFIED (0148 recorded an empty list)
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
