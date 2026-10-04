package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	tunnelsv1alpha1 "github.com/chenhunghan/flare-operator/api/tunnels/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
)

// Cloudflare Tunnel API calls (x-fern-sdk-group-name "tunnels"). Shapes follow recordings
// 0040 (create), 0041 (token), 0043/0097/0101 (get), 0095/0099/0207 (delete), 0102 (list).

// apiTunnel is the tunnel object of the create/get/list/delete responses.
type apiTunnel struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Status          string  `json:"status"`
	ConfigSrc       string  `json:"config_src"`
	RemoteConfig    *bool   `json:"remote_config"`
	TunType         string  `json:"tun_type"`
	CreatedAt       string  `json:"created_at"`
	DeletedAt       *string `json:"deleted_at"`
	ConnsActiveAt   *string `json:"conns_active_at"`
	ConnsInactiveAt *string `json:"conns_inactive_at"`
	// Token is only in the create response (0040).
	Token string `json:"token,omitempty"`
}

// deleted reports a soft-deleted tunnel: GET still answers 200 with deleted_at set (0101, 0208).
func (t *apiTunnel) deleted() bool { return t.DeletedAt != nil && *t.DeletedAt != "" }

// connected reports whether cloudflared connections are (or may still be) open: status healthy
// (0043) or degraded, or conns_active_at still set. After the connectors stop the status turns
// "down" and conns_active_at null (0097, 0207).
func (t *apiTunnel) connected() bool {
	return t.Status == "healthy" || t.Status == "degraded" || (t.ConnsActiveAt != nil && *t.ConnsActiveAt != "")
}

func (t *apiTunnel) observation() tunnelsv1alpha1.TunnelObservation {
	return tunnelsv1alpha1.TunnelObservation{
		ID: t.ID, Name: t.Name, Status: t.Status, ConfigSrc: t.ConfigSrc, RemoteConfig: t.RemoteConfig,
		TunType: t.TunType, CreatedAt: t.CreatedAt, DeletedAt: t.DeletedAt,
		ConnsActiveAt: t.ConnsActiveAt, ConnsInactiveAt: t.ConnsInactiveAt,
	}
}

func tunnelsPath(accountID string) string { return "/accounts/" + accountID + "/cfd_tunnel" }

func tunnelPath(accountID, id string) string {
	return tunnelsPath(accountID) + "/" + url.PathEscape(id)
}

func decodeTunnel(resp *cfclient.Response) (*apiTunnel, error) {
	var t apiTunnel
	if err := json.Unmarshal(resp.Result, &t); err != nil {
		return nil, fmt.Errorf("decode tunnel: %w", err)
	}
	return &t, nil
}

// getTunnel returns the tunnel, or nil when it does not exist or was soft-deleted.
func getTunnel(ctx context.Context, cf cfclient.Client, accountID, id string) (*apiTunnel, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: tunnelPath(accountID, id)})
	if err != nil {
		if cfclient.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	t, err := decodeTunnel(resp)
	if err != nil {
		return nil, err
	}
	if t.deleted() {
		return nil, nil
	}
	return t, nil
}

// findTunnelByName lists live tunnels named name (0102: name and is_deleted filters). It
// returns nil when there is none and an error when there are several (Cloudflare allows
// duplicate names; the emulator does too, UNVERIFIED).
func findTunnelByName(ctx context.Context, cf cfclient.Client, accountID, name string) (*apiTunnel, error) {
	q := url.Values{"name": {name}, "is_deleted": {"false"}}
	resp, err := cf.Do(cfclient.WithoutCache(ctx), cfclient.Request{Method: http.MethodGet, Path: tunnelsPath(accountID), Query: q})
	if err != nil {
		return nil, err
	}
	var list []apiTunnel
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		return nil, fmt.Errorf("decode tunnel list: %w", err)
	}
	var match []apiTunnel
	for _, t := range list {
		if t.Name == name && !t.deleted() {
			match = append(match, t)
		}
	}
	switch len(match) {
	case 0:
		return nil, nil
	case 1:
		return &match[0], nil
	}
	return nil, fmt.Errorf("%d live tunnels are named %q; pin one with the %s annotation: %w", len(match), name, "flare.dev/external-id", reconcile.ErrAmbiguousName)
}

// createTunnel creates a remotely managed tunnel (config_src=cloudflare, 0174). The response
// carries the connector token (0040).
func createTunnel(ctx context.Context, cf cfclient.Client, accountID, name string) (*apiTunnel, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: tunnelsPath(accountID),
		Body: map[string]string{"name": name, "config_src": "cloudflare"}})
	if err != nil {
		return nil, err
	}
	return decodeTunnel(resp)
}

// getToken returns the connector token; the result is a bare JSON string (0041).
func getToken(ctx context.Context, cf cfclient.Client, accountID, id string) (string, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: tunnelPath(accountID, id) + "/token"})
	if err != nil {
		return "", err
	}
	var tok string
	if err := json.Unmarshal(resp.Result, &tok); err != nil {
		return "", fmt.Errorf("decode tunnel token: %w", err)
	}
	return tok, nil
}

// codeActiveConnections is the delete refusal while cloudflared is connected (0095).
const codeActiveConnections = 1022

// deleteTunnel soft-deletes the tunnel (0099, 0207). DELETE sends no body.
func deleteTunnel(ctx context.Context, cf cfclient.Client, accountID, id string) error {
	_, err := cf.Do(ctx, cfclient.Request{Method: http.MethodDelete, Path: tunnelPath(accountID, id)})
	return err
}
