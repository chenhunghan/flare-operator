//go:build differential

package differential

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"flare.dev/operator/test/differential/harness"
)

// Shims for wrangler discrepancies. Each one answers a route flarefake does not emulate (or
// answers wrongly) with the smallest response that lets wrangler continue, built from
// flarefake's own state, so the steps after it still exercise flarefake. Each is registered
// only together with the Discrepancy it names (see wrangler_test.go).

var servicePath = regexp.MustCompile(`^/accounts/([^/]+)/workers/services/([^/]+)$`)

// shimServiceGet answers GET …/workers/services/{name} from flarefake's script list: 404/10007
// for a missing script (what wrangler's isWorkerNotFoundError accepts), else a
// default_environment carrying the script's tag.
func shimServiceGet(f *harness.Fake, id string) harness.Shim {
	return harness.Shim{
		Discrepancy: id, Method: http.MethodGet, Path: servicePath,
		Serve: func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) {
			m := servicePath.FindStringSubmatch(strings.TrimPrefix(r.URL.Path, "/client/v4"))
			acct, name := m[1], m[2]
			script := findScript(f, acct, name)
			if script == nil {
				harness.WriteEnvelope(w, http.StatusNotFound, nil, harness.APIError{Code: 10007, Message: "This Worker does not exist on your account."})
				return
			}
			harness.WriteEnvelope(w, http.StatusOK, map[string]any{
				"id":         name,
				"created_on": script["created_on"], "modified_on": script["modified_on"],
				"default_environment": map[string]any{
					"environment": "production",
					"created_on":  script["created_on"], "modified_on": script["modified_on"],
					"script": map[string]any{
						"id": name, "tag": script["tag"], "etag": script["etag"],
						"handlers": script["handlers"], "tags": script["tags"],
						"created_on": script["created_on"], "modified_on": script["modified_on"],
						"last_deployed_from": "wrangler",
					},
				},
			})
		},
	}
}

// shimServiceDelete maps DELETE …/workers/services/{name}?force=… onto flarefake's
// DELETE …/workers/scripts/{name}.
func shimServiceDelete(id string) harness.Shim {
	return harness.Shim{
		Discrepancy: id, Method: http.MethodDelete, Path: servicePath,
		Serve: func(w http.ResponseWriter, r *http.Request, body []byte, next http.Handler) {
			m := servicePath.FindStringSubmatch(strings.TrimPrefix(r.URL.Path, "/client/v4"))
			harness.Forward(w, r, body, next, "/accounts/"+m[1]+"/workers/scripts/"+m[2], "")
		},
	}
}

// shimScriptSub answers METHOD …/workers/scripts/{name}/<sub> with 404/10007 when flarefake has
// no such script and with 200 and result otherwise.
func shimScriptSub(f *harness.Fake, id, method, sub string, result any) harness.Shim {
	re := regexp.MustCompile(`^/accounts/([^/]+)/workers/scripts/([^/]+)/` + sub + `$`)
	return harness.Shim{
		Discrepancy: id, Method: method, Path: re,
		Serve: func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) {
			m := re.FindStringSubmatch(strings.TrimPrefix(r.URL.Path, "/client/v4"))
			if findScript(f, m[1], m[2]) == nil {
				harness.WriteEnvelope(w, http.StatusNotFound, nil, harness.APIError{Code: 10007, Message: "This Worker does not exist on your account."})
				return
			}
			harness.WriteEnvelope(w, http.StatusOK, result)
		},
	}
}

var workerPath = regexp.MustCompile(`^/accounts/([^/]+)/workers/workers/([^/]+)$`)

// shimWorkerGet answers GET …/workers/workers/{name} (the Workers resource API) with the
// fields wrangler reads (subdomain.enabled / previews_enabled), taken from flarefake's
// GET …/workers/scripts/{name}/subdomain.
func shimWorkerGet(f *harness.Fake, id string) harness.Shim {
	return harness.Shim{
		Discrepancy: id, Method: http.MethodGet, Path: workerPath,
		Serve: func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) {
			m := workerPath.FindStringSubmatch(strings.TrimPrefix(r.URL.Path, "/client/v4"))
			script := findScript(f, m[1], m[2])
			if script == nil {
				harness.WriteEnvelope(w, http.StatusNotFound, nil, harness.APIError{Code: 10007, Message: "This Worker does not exist on your account."})
				return
			}
			var sub map[string]any
			if status, env := f.Call(http.MethodGet, "/accounts/"+m[1]+"/workers/scripts/"+m[2]+"/subdomain", nil); status == http.StatusOK {
				_ = json.Unmarshal(env.Result, &sub)
			}
			harness.WriteEnvelope(w, http.StatusOK, map[string]any{
				"id": script["tag"], "name": m[2], "subdomain": sub,
				"created_on": script["created_on"], "updated_on": script["modified_on"],
			})
		},
	}
}

// findScript returns flarefake's list entry for a script, or nil.
func findScript(f *harness.Fake, acct, name string) map[string]any {
	status, env := f.Call(http.MethodGet, "/accounts/"+acct+"/workers/scripts", nil)
	if status != http.StatusOK {
		return nil
	}
	var list []map[string]any
	_ = json.Unmarshal(env.Result, &list)
	for _, s := range list {
		if s["id"] == name {
			return s
		}
	}
	return nil
}
