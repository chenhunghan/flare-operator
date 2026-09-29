package vpcservice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/cfclient"
)

// Workers VPC connectivity service API calls (x-fern-sdk-group-name "workers-vpc.services").
// Shapes follow recordings 0052/0055/0056/0176 (create), 0066/0186/0193 (PUT, a full replace),
// 0058/0111 (404/5104), 0110 (delete) and 0112 (list: no result_info).

type apiNetwork struct {
	TunnelID    string   `json:"tunnel_id"`
	ResolverIPs []string `json:"resolver_ips,omitempty"`
}

type apiHost struct {
	IPv4            *string     `json:"ipv4,omitempty"`
	IPv6            *string     `json:"ipv6,omitempty"`
	Hostname        *string     `json:"hostname,omitempty"`
	Network         *apiNetwork `json:"network,omitempty"`
	ResolverNetwork *apiNetwork `json:"resolver_network,omitempty"`
}

type apiTLS struct {
	CertVerificationMode string `json:"cert_verification_mode"`
}

// apiService is both the request body (read-only fields empty and omitted) and the response.
type apiService struct {
	ServiceID   string  `json:"service_id,omitempty"`
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	Host        apiHost `json:"host"`
	HTTPPort    *int32  `json:"http_port,omitempty"`
	HTTPSPort   *int32  `json:"https_port,omitempty"`
	TCPPort     *int32  `json:"tcp_port,omitempty"`
	AppProtocol *string `json:"app_protocol,omitempty"`
	// tls_settings is echoed only when set (0193; absent in 0052).
	TLSSettings *apiTLS `json:"tls_settings,omitempty"`
	CreatedAt   string  `json:"created_at,omitempty"`
	UpdatedAt   string  `json:"updated_at,omitempty"`
}

func servicesPath(accountID string) string {
	return "/accounts/" + accountID + "/connectivity/directory/services"
}

func servicePath(accountID, id string) string {
	return servicesPath(accountID) + "/" + url.PathEscape(id)
}

func decodeService(resp *cfclient.Response) (*apiService, error) {
	var s apiService
	if err := json.Unmarshal(resp.Result, &s); err != nil {
		return nil, fmt.Errorf("decode VPC service: %w", err)
	}
	return &s, nil
}

// getService returns the service, or nil on 404 (5104, 0058).
func getService(ctx context.Context, cf cfclient.Client, accountID, id string) (*apiService, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: servicePath(accountID, id)})
	if err != nil {
		if cfclient.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return decodeService(resp)
}

// codeInvalidParameters is the code of a refused create, a duplicate name among other causes
// (0059: 400/5101 "request contained invalid parameters: Service name '…' already exists").
const codeInvalidParameters = 5101

// isDuplicateName reports the API's refusal of a create because the name is taken (0059).
func isDuplicateName(err error) bool {
	ae, ok := cfclient.AsAPIError(err)
	if !ok || ae.Status != http.StatusBadRequest {
		return false
	}
	for _, d := range ae.Errors {
		if d.Code == codeInvalidParameters && strings.Contains(d.Message, "already exists") {
			return true
		}
	}
	return false
}

// findServiceByName lists the account's services (0112: one unpaginated array) and returns the
// one named name. Names are unique per account (5101 on duplicates, 0059).
func findServiceByName(ctx context.Context, cf cfclient.Client, accountID, name string) (*apiService, error) {
	resp, err := cf.Do(cfclient.WithoutCache(ctx), cfclient.Request{Method: http.MethodGet, Path: servicesPath(accountID)})
	if err != nil {
		return nil, err
	}
	var list []apiService
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		return nil, fmt.Errorf("decode VPC service list: %w", err)
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, nil
}

func createService(ctx context.Context, cf cfclient.Client, accountID string, body *apiService) (*apiService, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: servicesPath(accountID), Body: body})
	if err != nil {
		return nil, err
	}
	return decodeService(resp)
}

// replaceService PUTs the whole desired body (a full replace that also resets created_at, 0066;
// never send a partial PUT).
func replaceService(ctx context.Context, cf cfclient.Client, accountID, id string, body *apiService) (*apiService, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodPut, Path: servicePath(accountID, id), Body: body})
	if err != nil {
		return nil, err
	}
	return decodeService(resp)
}

// deleteService deletes the service (0110). Cloudflare allows it while a Worker still binds the
// service (0089). DELETE sends no body.
func deleteService(ctx context.Context, cf cfclient.Client, accountID, id string) error {
	_, err := cf.Do(ctx, cfclient.Request{Method: http.MethodDelete, Path: servicePath(accountID, id)})
	return err
}

func (s *apiService) observation() workersvpcv1alpha1.VPCServiceObservation {
	o := workersvpcv1alpha1.VPCServiceObservation{
		ServiceID: s.ServiceID, Name: s.Name, Type: s.Type,
		HTTPPort: s.HTTPPort, HTTPSPort: s.HTTPSPort, TCPPort: s.TCPPort, AppProtocol: s.AppProtocol,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
		Host: &workersvpcv1alpha1.VPCServiceHostObservation{IPv4: s.Host.IPv4, IPv6: s.Host.IPv6, Hostname: s.Host.Hostname},
	}
	if s.TLSSettings != nil {
		o.TLSSettings = &workersvpcv1alpha1.VPCServiceTLSSettings{CertVerificationMode: s.TLSSettings.CertVerificationMode}
	}
	if n := s.Host.Network; n != nil {
		o.Host.Network = &workersvpcv1alpha1.VPCServiceNetworkObservation{TunnelID: n.TunnelID, ResolverIPs: n.ResolverIPs}
	}
	if n := s.Host.ResolverNetwork; n != nil {
		o.Host.ResolverNetwork = &workersvpcv1alpha1.VPCServiceNetworkObservation{TunnelID: n.TunnelID, ResolverIPs: n.ResolverIPs}
	}
	return o
}

func eqStr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// eqOptional compares a desired optional value with the observed one; an unset desired value
// matches anything (the API may default it; UNVERIFIED whether it does for ports and
// app_protocol, the emulator echoes null).
func eqOptional[T comparable](want, have *T) bool {
	return want == nil || (have != nil && *want == *have)
}

func eqNetwork(want, have *apiNetwork) bool {
	if want == nil || have == nil {
		return want == nil && have == nil
	}
	if want.TunnelID != have.TunnelID || len(want.ResolverIPs) != len(have.ResolverIPs) {
		return false
	}
	for i := range want.ResolverIPs {
		if want.ResolverIPs[i] != have.ResolverIPs[i] {
			return false
		}
	}
	return true
}

// matches reports whether the observed service already equals the desired body.
func matches(want, have *apiService) bool {
	tlsEq := (want.TLSSettings == nil && have.TLSSettings == nil) ||
		(want.TLSSettings != nil && have.TLSSettings != nil && *want.TLSSettings == *have.TLSSettings)
	return want.Name == have.Name && want.Type == have.Type &&
		eqStr(want.Host.Hostname, have.Host.Hostname) && eqStr(want.Host.IPv4, have.Host.IPv4) && eqStr(want.Host.IPv6, have.Host.IPv6) &&
		eqNetwork(want.Host.Network, have.Host.Network) && eqNetwork(want.Host.ResolverNetwork, have.Host.ResolverNetwork) &&
		eqOptional(want.HTTPPort, have.HTTPPort) && eqOptional(want.HTTPSPort, have.HTTPSPort) &&
		eqOptional(want.TCPPort, have.TCPPort) && eqOptional(want.AppProtocol, have.AppProtocol) && tlsEq
}
