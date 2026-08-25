package strategy

// brain.go implements RegimeBrain: a lightweight online logistic-regression
// classifier that learns to distinguish "explosive" BTC-gap windows from
// dormant ones.  It requires zero external dependencies.
//
// # How it works
//
//  1. At every EvaluatePairArb call (even when no signal fires) a feature
//     snapshot is stored in a pending buffer together with the BTC price at
//     capture time.
//
//  2. On every incoming BTC tick (OnBitstampTrade) MaybeLabel is called.
//     Pending entries that are >= labelHorizonSec old are retroactively labeled:
//     if the absolute BTC-vs-open gap grew by >= labelThresholdUSD the sample
//     is marked explosive (y=1), otherwise dormant (y=0).
//
//  3. Labeled samples drive one mini-batch SGD pass that updates the logistic
//     weights.  Weights are also periodically saved to brain_state.json so they
//     survive restarts and accumulate knowledge across sessions.
//
//  4. When brain.Score(features) < scoreThreshold the pair-arb entry is blocked
//     no matter what the static filters say.  The brain stays fully permissive
//     (Score=1.0) until minSamplesScore labeled examples have been collected, so
//     the bot keeps trading normally while the model warms up.
//
// Features fed to the model (all normalised internally with running Welford stats):
//   - GapVelocity    signed gap velocity $/s (btcGapVelocityUSDPerSec(20))
//   - AbsGap         |btc − open| in USD
//   - CVD30s         net Binance BTC CVD over 30 s (signed)
//   - BookImbalance  Deribit near-mid imbalance [-1, 1]
//   - TickRate5s     Bitstamp BTC ticks per second over last 5 s
//   - WindowFraction fraction of window elapsed [0, 1]

import (
	"encoding/json"
	"math"
	"os"
	"sync"
	"time"
)

const brainNumFeatures = 6

// BrainFeatures is the feature vector snapshotted at each evaluation point.
type BrainFeatures struct {
	GapVelocity    float64 // signed $/s — btcGapVelocityUSDPerSec(20)
	AbsGap         float64 // |btc − open| in USD
	CVD30s         float64 // net Binance BTC CVD 30 s, signed
	BookImbalance  float64 // Deribit imbalance [-1, 1]
	TickRate5s     float64 // Bitstamp ticks per second (last 5 s)
	WindowFraction float64 // elapsed / total window duration [0, 1]
}

func (f BrainFeatures) toArray() [brainNumFeatures]float64 {
	return [brainNumFeatures]float64{
		f.GapVelocity, f.AbsGap, f.CVD30s,
		f.BookImbalance, f.TickRate5s, f.WindowFraction,
	}
}

// ── Running statistics (Welford online algorithm) ────────────────────────────

type runningStats struct {
	N    float64
	Mean float64
	M2   float64 // sum of squared deviations
}

func (rs *runningStats) update(x float64) {
	rs.N++
	delta := x - rs.Mean
	rs.Mean += delta / rs.N
	rs.M2 += delta * (x - rs.Mean)
}

func (rs *runningStats) std() float64 {
	if rs.N < 2 {
		return 1.0
	}
	v := rs.M2 / rs.N
	if v < 1e-9 {
		return 1.0
	}
	return math.Sqrt(v)
}

func (rs *runningStats) normalize(x float64) float64 {
	s := rs.std()
	if s == 0 {
		return 0
	}
	return (x - rs.Mean) / s
}

// ── Pending buffer ────────────────────────────────────────────────────────────

type pendingLabelEntry struct {
	at        time.Time
	btcPrice  float64
	openPrice float64
	features  BrainFeatures
}

type labeledExample struct {
	features BrainFeatures
	label    float64 // 1.0 = explosive, 0.0 = dormant
}

// ── Persisted state ───────────────────────────────────────────────────────────

type brainStateFile struct {
	Weights      [brainNumFeatures + 1]float64 `json:"weights"` // +1 for bias
	StatsN       [brainNumFeatures]float64     `json:"stats_n"`
	StatsMean    [brainNumFeatures]float64     `json:"stats_mean"`
	StatsM2      [brainNumFeatures]float64     `json:"stats_m2"`
	TotalLabeled int                           `json:"total_labeled"`
}

// ── RegimeBrain ───────────────────────────────────────────────────────────────

// BrainConfig holds all tuneable parameters for RegimeBrain.
type BrainConfig struct {
	// Enabled: when false the brain is a no-op (Score always returns 1.0).
	Enabled bool
	// StateFile is the path to the JSON persistence file (e.g. "brain_state.json").
	// Empty = no persistence.
	StateFile string
	// LearningRate for SGD (default 0.05).
	LearningRate float64
	// LabelHorizonSec: seconds to wait before labeling a pending entry (default 60).
	LabelHorizonSec float64
	// LabelThresholdUSD: the absolute gap must grow by at least this many USD in
	// LabelHorizonSec for the sample to be considered explosive (default 20.0).
	LabelThresholdUSD float64
	// MinSamplesScore: brain stays fully permissive until this many labeled
	// examples have been collected (default 30).
	MinSamplesScore int
	// ScoreThreshold: minimum brain score required to allow a pair-arb entry
	// (default 0.45). Set to 0 to disable gating while still collecting data.
	ScoreThreshold float64
	// MaxExamples: ring-buffer size for training examples (default 500).
	MaxExamples int
	// L2Lambda: L2 regularisation coefficient (default 0.001).
	L2Lambda float64
}

// RegimeBrain is a thread-safe online logistic regression classifier.
type RegimeBrain struct {
	mu sync.Mutex

	weights [brainNumFeatures + 1]float64 // last slot = bias weight
	stats   [brainNumFeatures]runningStats

	// Config (immutable after construction)
	enabled           bool
	stateFile         string
	learningRate      float64
	labelHorizonSec   float64
	labelThresholdUSD float64
	minSamplesScore   int
	scoreThreshold    float64
	maxExamples       int
	l2Lambda          float64

	// Pending retroactive labels
	pending []pendingLabelEntry

	// Labeled training ring-buffer
	examples     []labeledExample
	totalLabeled int
}

// NewRegimeBrain creates a RegimeBrain and loads any persisted state.
func NewRegimeBrain(cfg BrainConfig) *RegimeBrain {
	b := &RegimeBrain{
		enabled:           cfg.Enabled,
		stateFile:         cfg.StateFile,
		learningRate:      cfg.LearningRate,
		labelHorizonSec:   cfg.LabelHorizonSec,
		labelThresholdUSD: cfg.LabelThresholdUSD,
		minSamplesScore:   cfg.MinSamplesScore,
		scoreThreshold:    cfg.ScoreThreshold,
		maxExamples:       cfg.MaxExamples,
		l2Lambda:          cfg.L2Lambda,
	}
	if b.learningRate <= 0 {
		b.learningRate = 0.05
	}
	if b.labelHorizonSec <= 0 {
		b.labelHorizonSec = 60
	}
	if b.labelThresholdUSD <= 0 {
		b.labelThresholdUSD = 20
	}
	if b.minSamplesScore <= 0 {
		b.minSamplesScore = 30
	}
	if b.scoreThreshold <= 0 {
		b.scoreThreshold = 0.45
	}
	if b.maxExamples <= 0 {
		b.maxExamples = 500
	}
	if b.l2Lambda <= 0 {
		b.l2Lambda = 0.001
	}
	if b.enabled && b.stateFile != "" {
		b.load()
		if _, err := os.Stat(b.stateFile); err != nil {
			b.saveNoLock()
		}
	}
	return b
}

// Enabled returns whether the brain gate is active.
func (b *RegimeBrain) Enabled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.enabled
}

// ScoreThreshold returns the configured gate threshold.
func (b *RegimeBrain) ScoreThreshold() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.scoreThreshold
}

// TotalLabeled returns how many labeled examples have been processed so far.
func (b *RegimeBrain) TotalLabeled() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.totalLabeled
}

// Score returns the probability [0,1] of an explosive regime.
// Returns 1.0 (fully permissive) when the brain is disabled or not yet warm.
func (b *RegimeBrain) Score(f BrainFeatures) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.enabled || b.totalLabeled < b.minSamplesScore {
		return 1.0
	}
	v := b.buildVec(f)
	return brainSigmoid(brainDot(b.weights, v))
}

// AddObservation stores a feature snapshot for future retroactive labeling.
// Call this at every eval point, regardless of whether a signal fires.
// btcPrice and openPrice are the BTC and window-open prices at capture time.
func (b *RegimeBrain) AddObservation(f BrainFeatures, btcPrice, openPrice float64, at time.Time) {
	if !b.enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Update running normalisation statistics.
	arr := f.toArray()
	for i, x := range arr {
		b.stats[i].update(x)
	}

	b.pending = append(b.pending, pendingLabelEntry{
		at:        at,
		btcPrice:  btcPrice,
		openPrice: openPrice,
		features:  f,
	})
}

// MaybeLabel scans the pending buffer and retroactively labels entries that are
// old enough (>= labelHorizonSec).  Call this on every new BTC price tick.
func (b *RegimeBrain) MaybeLabel(currentBTCPrice float64, now time.Time) {
	if !b.enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	horizon := time.Duration(b.labelHorizonSec * float64(time.Second))
	cutoff := now.Add(-horizon)

	var remaining []pendingLabelEntry
	var batch []labeledExample

	for _, p := range b.pending {
		if p.at.After(cutoff) {
			remaining = append(remaining, p)
			continue
		}
		// Label: did |gap| grow by >= threshold within labelHorizonSec?
		gapAtCapture := math.Abs(p.btcPrice - p.openPrice)
		gapNow := math.Abs(currentBTCPrice - p.openPrice)
		var label float64
		if gapNow-gapAtCapture >= b.labelThresholdUSD {
			label = 1.0
		}
		batch = append(batch, labeledExample{features: p.features, label: label})
	}
	b.pending = remaining

	if len(batch) == 0 {
		return
	}

	// Add to ring buffer.
	for _, ex := range batch {
		if len(b.examples) >= b.maxExamples {
			b.examples = b.examples[1:]
		}
		b.examples = append(b.examples, ex)
		b.totalLabeled++
	}

	// One SGD pass over the newly labeled batch.
	b.trainBatch(batch)

	// Persist every 50 new labels.
	if b.stateFile != "" && b.totalLabeled%50 == 0 {
		b.saveNoLock()
	}
}

// Save flushes the current state to disk immediately.
func (b *RegimeBrain) Save() {
	if !b.enabled || b.stateFile == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.saveNoLock()
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// buildVec returns the normalised feature vector with bias appended.
// Caller must hold b.mu.
func (b *RegimeBrain) buildVec(f BrainFeatures) [brainNumFeatures + 1]float64 {
	arr := f.toArray()
	var v [brainNumFeatures + 1]float64
	for i, x := range arr {
		v[i] = b.stats[i].normalize(x)
	}
	v[brainNumFeatures] = 1.0 // bias term
	return v
}

// trainBatch performs one SGD pass over the provided examples.
// Caller must hold b.mu.
func (b *RegimeBrain) trainBatch(examples []labeledExample) {
	for _, ex := range examples {
		v := b.buildVec(ex.features)
		pred := brainSigmoid(brainDot(b.weights, v))
		errVal := pred - ex.label
		for i := range b.weights {
			grad := errVal * v[i]
			if i < brainNumFeatures { // L2 regularisation (not applied to bias)
				grad += b.l2Lambda * b.weights[i]
			}
			b.weights[i] -= b.learningRate * grad
		}
	}
}

// load restores persisted state from the state file.
// Called once at construction; ignores errors (fresh start if file is absent).
func (b *RegimeBrain) load() {
	data, err := os.ReadFile(b.stateFile)
	if err != nil {
		return
	}
	var s brainStateFile
	if err := json.Unmarshal(data, &s); err != nil {
		return
	}
	b.weights = s.Weights
	b.totalLabeled = s.TotalLabeled
	for i := 0; i < brainNumFeatures; i++ {
		b.stats[i].N = s.StatsN[i]
		b.stats[i].Mean = s.StatsMean[i]
		b.stats[i].M2 = s.StatsM2[i]
	}
}

// saveNoLock writes current state to the state file.  Caller must hold b.mu.
func (b *RegimeBrain) saveNoLock() {
	s := brainStateFile{
		Weights:      b.weights,
		TotalLabeled: b.totalLabeled,
	}
	for i := 0; i < brainNumFeatures; i++ {
		s.StatsN[i] = b.stats[i].N
		s.StatsMean[i] = b.stats[i].Mean
		s.StatsM2[i] = b.stats[i].M2
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(b.stateFile, data, 0644)
}

// brainSigmoid is a numerically stable sigmoid.
func brainSigmoid(x float64) float64 {
	if x >= 0 {
		e := math.Exp(-x)
		return 1.0 / (1.0 + e)
	}
	e := math.Exp(x)
	return e / (1.0 + e)
}

// brainDot computes the dot product of two [brainNumFeatures+1] vectors.
func brainDot(a, b [brainNumFeatures + 1]float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}
