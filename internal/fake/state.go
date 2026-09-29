package fake

import "sort"

// account holds all emulated resources of one Cloudflare account.
type account struct {
	id      string
	kv      map[string]*kvNamespace
	d1      map[string]*d1Database
	queues  map[string]*queue
	tunnels map[string]*tunnel
	vnets   map[string]*virtualNetwork
	vpc     map[string]*vpcService
	scripts map[string]*workerScript
}

func (s *Server) accountLocked(id string) *account {
	a, ok := s.accounts[id]
	if !ok {
		a = &account{
			id:      id,
			kv:      map[string]*kvNamespace{},
			d1:      map[string]*d1Database{},
			queues:  map[string]*queue{},
			tunnels: map[string]*tunnel{},
			vnets:   map[string]*virtualNetwork{},
			vpc:     map[string]*vpcService{},
			scripts: map[string]*workerScript{},
		}
		s.accounts[id] = a
	}
	return a
}

// sortedValues returns map values sorted by key (IDs), the order the real API lists KV
// namespaces in by default (recordings 0007/0008 and kv-order-*).
func sortedValues[T any](m map[string]*T) []*T {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*T, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// sortedBySeq returns values in creation order (by the server's creation sequence number), so
// lists are deterministic even when resources are created at the same (frozen) instant. Real
// D1/Queues/Tunnel list ordering is UNVERIFIED; only KV's (ID ascending) is recorded.
func sortedBySeq[T any](m map[string]*T, seq func(*T) int64) []*T {
	out := sortedValues(m)
	sort.SliceStable(out, func(i, j int) bool { return seq(out[i]) < seq(out[j]) })
	return out
}
