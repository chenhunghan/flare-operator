package fake

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// idSource generates Cloudflare-shaped identifiers. Tests can queue exact IDs (POST /_fake/ids)
// so that replayed recordings produce byte-identical results, including list ordering.
type idSource struct {
	mu     sync.Mutex
	queued []string
}

func (s *idSource) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queued = nil
}

// pending reports how many queued IDs have not been consumed.
func (s *idSource) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queued)
}

func (s *idSource) enqueue(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queued = append(s.queued, ids...)
}

func (s *idSource) next(gen func() string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queued) > 0 {
		id := s.queued[0]
		s.queued = s.queued[1:]
		return id
	}
	return gen()
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// hex32 is the 32-hex-char form used by KV namespaces and Queues.
func hex32() string { return hex.EncodeToString(randBytes(16)) }

// uuidV4 is used by D1 databases and tunnels.
func uuidV4() string {
	b := randBytes(16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

// uuidV7 is used by Workers VPC services (recording 0052: "01a0ebf6-0a78-73c3-…").
func uuidV7(now time.Time) string {
	b := randBytes(16)
	ms := uint64(now.UnixMilli())
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(b[0:6], ts[2:8])
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

func formatUUID(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
