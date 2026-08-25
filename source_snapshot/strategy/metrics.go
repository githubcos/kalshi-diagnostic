package strategy

import (
	"encoding/json"
	"math"
	"sort"
	"sync"
	"time"
)

// metricsWindowSize is the maximum number of samples retained per operation
// for rolling percentile computation.
const metricsWindowSize = 200

type rollingBuf struct {
	data []int64
	pos  int
	n    int64 // total samples ever recorded
}

func (b *rollingBuf) add(v int64) {
	if len(b.data) < metricsWindowSize {
		b.data = append(b.data, v)
	} else {
		b.data[b.pos] = v
	}
	b.pos = (b.pos + 1) % metricsWindowSize
	b.n++
}

func (b *rollingBuf) stats() (count int64, avg, p50, p99, minV, maxV float64) {
	n := len(b.data)
	if n == 0 {
		return 0, 0, 0, 0, 0, 0
	}
	count = b.n
	tmp := make([]int64, n)
	copy(tmp, b.data)
	sort.Slice(tmp, func(i, j int) bool { return tmp[i] < tmp[j] })
	sum := int64(0)
	for _, v := range tmp {
		sum += v
	}
	avg = float64(sum) / float64(n)
	minV = float64(tmp[0])
	maxV = float64(tmp[n-1])
	p50Idx := int(math.Floor(float64(n) * 0.50))
	if p50Idx >= n {
		p50Idx = n - 1
	}
	p99Idx := int(math.Floor(float64(n) * 0.99))
	if p99Idx >= n {
		p99Idx = n - 1
	}
	p50 = float64(tmp[p50Idx])
	p99 = float64(tmp[p99Idx])
	return
}

type opBucket struct {
	buf    rollingBuf
	lastMs int64
	lastAt time.Time
}

// BotMetrics collects rolling latency statistics for key bot operations.
// All methods are safe for concurrent use. A nil receiver is a safe no-op.
type BotMetrics struct {
	mu  sync.Mutex
	ops map[string]*opBucket
}

// NewBotMetrics creates a new metrics collector.
func NewBotMetrics() *BotMetrics {
	return &BotMetrics{ops: make(map[string]*opBucket)}
}

// Record adds a latency sample (in milliseconds) for the named operation.
func (m *BotMetrics) Record(op string, ms int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	b, ok := m.ops[op]
	if !ok {
		b = &opBucket{}
		m.ops[op] = b
	}
	b.buf.add(ms)
	b.lastMs = ms
	b.lastAt = time.Now()
	m.mu.Unlock()
}

// OpSnapshot is the JSON-serialisable snapshot for a single operation.
type OpSnapshot struct {
	Op     string  `json:"op"`
	Count  int64   `json:"count"`
	AvgMs  float64 `json:"avg_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MinMs  float64 `json:"min_ms"`
	MaxMs  float64 `json:"max_ms"`
	LastMs int64   `json:"last_ms"`
	LastAt string  `json:"last_at"`
}

// Snapshot returns a JSON byte slice (array of OpSnapshot sorted by op name).
func (m *BotMetrics) Snapshot() []byte {
	if m == nil {
		b, _ := json.Marshal([]OpSnapshot{})
		return b
	}
	m.mu.Lock()
	out := make([]OpSnapshot, 0, len(m.ops))
	for op, b := range m.ops {
		count, avg, p50, p99, minV, maxV := b.buf.stats()
		lastAt := ""
		if !b.lastAt.IsZero() {
			lastAt = b.lastAt.UTC().Format(time.RFC3339)
		}
		out = append(out, OpSnapshot{
			Op:     op,
			Count:  count,
			AvgMs:  math.Round(avg*10) / 10,
			P50Ms:  p50,
			P99Ms:  p99,
			MinMs:  minV,
			MaxMs:  maxV,
			LastMs: b.lastMs,
			LastAt: lastAt,
		})
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Op < out[j].Op })
	data, _ := json.Marshal(out)
	return data
}
