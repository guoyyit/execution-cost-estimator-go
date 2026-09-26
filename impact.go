package estimator

import (
	"math"
	"math/rand"
	"time"
)

// impactModel projects deterministic, reproducible slice costs.
//
// Cost model (all costs are in quote currency and reported as non-negative
// magnitudes for both sides):
//
//	slippage_i  = q_i * |vwap_i - mid| / mid * mid
//	             = q_i * |vwap_i - mid|
//	             pure order-book walk cost (spread + depth consumption).
//	impact_i    = q_i * mid * (temporary_i + persistent_i)
//	temporary_i = eta * participation_i^gamma * (1 + noise_i)
//	persistent  = sum_{j<i} eta * participation_j^gamma * (1-decay)^(i-j)
//	risk_i      = remain_i * mid * sigma * sqrt(t_remain) * riskAversion
//
// participation_i is the slice size relative to total visible same-side depth
// (an ADV proxy). noise_i is a reproducible normal draw scaled by 0.1*sigma,
// driven solely by the plan seed: identical (request, snapshot, seed) yields
// identical slice-by-slice output.
type impactModel struct {
	cfg Config
}

func newImpactModel(cfg Config) *impactModel {
	return &impactModel{cfg: cfg}
}

// numSlices derives slice count from the horizon and caps it.
func (m *impactModel) numSlices(horizonSeconds float64) int {
	min := m.cfg.MinSliceSeconds
	if min <= 0 {
		min = 1
	}
	n := int(math.Ceil(horizonSeconds/min + 1e-9))
	if n < 1 {
		n = 1
	}
	if m.cfg.MaxSlices > 0 && n > m.cfg.MaxSlices {
		n = m.cfg.MaxSlices
	}
	return n
}

// sliceWeights returns the fraction of total quantity per slice; values sum
// to 1 exactly.
func (m *impactModel) sliceWeights(strategy Strategy, n int) []float64 {
	raw := make([]float64, n)
	switch strategy {
	case VWAP:
		// U-shaped intraday profile: heavy at the open/close, light midday.
		for i := 0; i < n; i++ {
			x := float64(i) / float64(n)
			raw[i] = 1 - 0.6*4*x*(1-x)
		}
	case IS:
		// Implementation shortfall: exponential front-loading.
		for i := 0; i < n; i++ {
			raw[i] = math.Exp(-1.0 * float64(i))
		}
	default: // TWAP and any unknown strategy fall back to equal slices.
		for i := range raw {
			raw[i] = 1
		}
	}
	sum := 0.0
	for _, v := range raw {
		sum += v
	}
	w := make([]float64, n)
	acc := 0.0
	for i := range w {
		w[i] = raw[i] / sum
		acc += w[i]
	}
	// Pin the residual onto the largest weight so the sum is exactly 1.
	w[0] += 1 - acc
	return w
}

// rng builds the deterministic RNG for one plan.
func (m *impactModel) rng(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed))
}

// totalSideSize sums visible size on the side hit by the order.
func totalSideSize(side Side, snap DepthSnapshot) float64 {
	total := 0.0
	for _, l := range snap.levels(side) {
		total += l.Size
	}
	return total
}

// project computes every slice's expected cost from one consistent snapshot.
func (m *impactModel) project(req EstimateRequest, snap DepthSnapshot, weights []float64, r *rand.Rand) ([]SliceReport, error) {
	n := len(weights)
	adv := totalSideSize(req.Side, snap)
	if adv <= 0 {
		return nil, ErrDepthUnavailable
	}
	mid := snap.Mid
	sigma := m.cfg.Volatility
	eta := m.cfg.ImpactCoeff
	gamma := m.cfg.ImpactExponent
	if gamma <= 0 {
		gamma = 0.5
	}

	// Pre-compute the participation footprint so persistent impact can see
	// every slice in schedule order.
	parts := make([]float64, n)
	imps := make([]float64, n)
	noiseSigma := 0.1 * sigma
	for i, w := range weights {
		q := req.Quantity * w
		parts[i] = q / adv
		imps[i] = eta * math.Pow(parts[i], gamma)
	}

	out := make([]SliceReport, n)
	sliceSeconds := req.Horizon.Seconds() / float64(n)
	decay := m.cfg.DecayRate
	if decay < 0 {
		decay = 0
	}
	if decay > 1 {
		decay = 1
	}
	remainAfter := req.Quantity
	for i := 0; i < n; i++ {
		q := req.Quantity * weights[i]
		vwap, err := avgFillPrice(req.Side, snap, q)
		if err != nil {
			return nil, err
		}
		slipUnit := math.Abs(vwap-mid) / mid

		// Persistent impact inherited from earlier slices.
		persistent := 0.0
		for j := 0; j < i; j++ {
			persistent += imps[j] * math.Pow(1-decay, float64(i-j))
		}
		noise := r.NormFloat64() * noiseSigma
		temp := imps[i] * (1 + noise)

		slippage := slipUnit * mid * q
		impact := (temp + persistent) * mid * q
		remainAfter -= q
		tFrac := float64(n-1-i) / float64(n)
		risk := remainAfter * mid * sigma * math.Sqrt(tFrac) * m.cfg.RiskAversion

		out[i] = SliceReport{
			Index:        i,
			Offset:       durationFromSeconds(float64(i) * sliceSeconds),
			Quantity:     q,
			Mid:          mid,
			FillPrice:    vwap,
			SlippageCost: slippage,
			ImpactCost:   impact,
			TotalCost:    slippage + impact,
			ResidualRisk: risk,
			Status:       SlicePending,
		}
	}
	return out, nil
}

// avgFillPrice walks the book for a market quantity and returns the
// volume-weighted average fill price.
func avgFillPrice(side Side, snap DepthSnapshot, qty float64) (vwap float64, err error) {
	if qty <= 0 {
		return 0, ErrInvalidArgument
	}
	tiers, ok := walk(side, snap, qty)
	if !ok {
		return 0, ErrDepthInsufficient
	}
	notional, filled := 0.0, 0.0
	for _, t := range tiers {
		notional += t.Price * t.Size
		filled += t.Size
	}
	if filled <= 0 {
		return 0, ErrDepthInsufficient
	}
	return notional / filled, nil
}

func durationFromSeconds(s float64) time.Duration {
	return time.Duration(s * secondsToNanos)
}

const secondsToNanos = 1e9
