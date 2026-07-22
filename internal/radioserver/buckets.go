package radioserver

import (
	"sync"
	"time"
)

const (
	bucketCapacity = 10
	refillPer      = 6 * time.Second // 1 token per 6s ≈ 10 searches/min
)

// buckets is a per-user token bucket guarding yt-dlp searches. In-memory
// on purpose: the service is single-replica (spec §6).
type buckets struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]*bucketState
}

type bucketState struct {
	tokens float64
	last   time.Time
}

func newBuckets(now func() time.Time) *buckets {
	return &buckets{now: now, m: map[string]*bucketState{}}
}

func (b *buckets) allow(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	st, ok := b.m[key]
	if !ok {
		st = &bucketState{tokens: bucketCapacity, last: now}
		b.m[key] = st
	}
	st.tokens += now.Sub(st.last).Seconds() / refillPer.Seconds()
	if st.tokens > bucketCapacity {
		st.tokens = bucketCapacity
	}
	st.last = now
	if st.tokens < 1 {
		return false
	}
	st.tokens--
	return true
}
