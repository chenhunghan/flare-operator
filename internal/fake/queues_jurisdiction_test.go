package fake

import "testing"

// UNVERIFIED (no recording creates a queue with a jurisdiction): a jurisdiction set at create
// is returned by create, GET, list and PATCH, and is absent when not set (0028).
func TestQueueJurisdictionReadBack(t *testing.T) {
	c := newClient(t, New(Options{}))
	st, env, _ := c.do("POST", acct+"/queues", map[string]string{"queue_name": "q-eu", "jurisdiction": "eu"})
	if st != 200 {
		t.Fatalf("create: %d", st)
	}
	res := env.Result.(map[string]any)
	if res["jurisdiction"] != "eu" {
		t.Fatalf("create result jurisdiction = %v", res["jurisdiction"])
	}
	id := res["queue_id"].(string)
	for _, method := range []string{"GET", "PATCH"} {
		var body any
		if method == "PATCH" {
			body = map[string]any{"settings": map[string]any{"delivery_delay": 3}}
		}
		st, env, _ := c.do(method, acct+"/queues/"+id, body)
		if st != 200 || env.Result.(map[string]any)["jurisdiction"] != "eu" {
			t.Errorf("%s: %d jurisdiction=%v", method, st, env.Result.(map[string]any)["jurisdiction"])
		}
	}
	_, env, _ = c.do("GET", acct+"/queues", nil)
	if items := env.Result.([]any); len(items) != 1 || items[0].(map[string]any)["jurisdiction"] != "eu" {
		t.Errorf("list: %v", items)
	}
	_, env, _ = c.do("POST", acct+"/queues", map[string]string{"queue_name": "q-plain"})
	if _, has := env.Result.(map[string]any)["jurisdiction"]; has {
		t.Error("jurisdiction returned for a queue created without one (0028 has none)")
	}
}
