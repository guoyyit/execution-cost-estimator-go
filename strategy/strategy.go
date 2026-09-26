// Package strategy turns an order request into deterministic execution
// slices with expected slippage, spread cost, market impact and
// residual risk.
//
// Model (linear, transparent and fully reproducible):
//
//	arrival           mid at estimation time (snapshot basis).
//	half-spread cost  h = (best_ask-best_bid)/2, paid on every unit.
//	temporary impact  I_i = eta * q_i / ADV_i, decaying after the slice
//	                  by decayRate: I contributes price drift while the
//	                  slice executes and relaxes afterwards.
//	noise             optional deterministic uniform noise driven by
//	                  Params.Seed; same seed => identical numbers.
//	expected price    side-adjusted best price +/- spread + impact -
//	                  post-decay of prior temporary impact + noise.
//	slippage          per-unit cost relative to arrival mid, always
//	                  expressed as a positive cost for the side.
//	residual risk     remaining notional * sigma * sqrt(remaining time).
package strategy

import (
	"math"
	"time"

	"github.com/guoyyit/execution-cost-estimator-go/model"
)

// Default parameters applied when a request leaves a field at zero.
const (
	DefaultImpactCoeff = 0.10
	DefaultDecayRate   = 0.50
	DefaultMaxSlices   = 20
	DefaultRiskAnnual  = 0.40
	DefaultParticipate = 0.10
	minutePerYear      = 525600.0
)

// Engine builds plans against a depth view.
type Engine struct{}

// NewEngine builds an Engine.
func NewEngine() *Engine { return &Engine{} }

// BuildPlan constructs the full slice schedule for a request. The view
// must be a complete consistent snapshot (the service guarantees this).
func (e *Engine) BuildPlan(req model.EstimateRequest, view model.DepthView, now time.Time) (*model.Plan, error) {
	if req.Symbol == "" || req.Quantity <= 0 || req.Horizon <= 0 {
		return nil, model.ErrInvalidArgument
	}
	if req.Side != model.SideBuy && req.Side != model.SideSell {
		return nil, model.ErrInvalidArgument
	}
	if req.Strategy == 0 {
		req.Strategy = model.StrategyTWAP
	}
	p := normalizeParams(req.Params)
	horizon := req.Horizon.Duration()

	arrival := view.MidPrice()
	if arrival <= 0 {
		return nil, model.ErrEmptyDepth
	}

	n := p.MaxSlices
	if n <= 0 {
		n = 1
	}
	if horizon > 0 {
		// One slice per horizon minute, capped by MaxSlices.
		byStep := int(horizon / time.Minute)
		if byStep < 1 {
			byStep = 1
		}
		if byStep < n {
			n = byStep
		}
	}

	slices := make([]model.Slice, n)
	step := horizon / time.Duration(n)
	if step <= 0 {
		step = horizon
	}

	levels := view.Asks
	if req.Side == model.SideSell {
		levels = view.Bids
	}
	// ADV proxy: total size visible on the executable side.
	adv := 0.0
	for _, l := range levels {
		adv += l.Size
	}
	if adv <= 0 {
		return nil, model.ErrEmptyDepth
	}

	// POV: each slice may absorb at most Participate*ADV, so the slice
	// count is participation-driven (bounded by MaxSlices).
	if req.Strategy == model.StrategyPOV {
		capPerSlice := p.Participate * adv
		if capPerSlice > 0 {
			need := int(math.Ceil(req.Quantity / capPerSlice))
			if need < 1 {
				need = 1
			}
			if need > p.MaxSlices {
				need = p.MaxSlices
			}
			n = need
			slices = make([]model.Slice, n)
			step = horizon / time.Duration(n)
			if step <= 0 {
				step = horizon
			}
		}
	}
	weights := sliceWeights(req.Strategy, n)

	// Deterministic noise stream, offset per plan slice by plan identity
	// (seed + index) so identical inputs reproduce identical numbers.
	rng := newRng(p.Seed)
	spread := view.Spread()
	halfSpread := spread / 2
	best := view.BestPrice(req.Side)
	sigma := p.RiskAnnual / math.Sqrt(minutePerYear) // per-minute vol

	// carryImpact is surviving temporary impact from prior slices.
	carryImpact := 0.0
	filled := 0.0
	for i := 0; i < n; i++ {
		q := req.Quantity * weights[i]
		if i == n-1 {
			q = req.Quantity - filled // absorb rounding on the last slice
		}
		filled += q

		impact := p.ImpactCoeff * (q / adv)
		// Prior impact decays once per step before the next slice prices.
		var price float64
		switch req.Side {
		case model.SideBuy:
			price = best + halfSpread + impact - carryImpact + rng.noise(p.RNG, arrival)
		case model.SideSell:
			price = best - halfSpread - impact + carryImpact - rng.noise(p.RNG, arrival)
		}
		slip := model.SlippageCost(req.Side, arrival, price)
		if slip < 0 {
			slip = 0
		}

		remaining := req.Quantity - filled
		remainingMinutes := horizon.Minutes() - float64(i+1)*step.Minutes()
		if remainingMinutes < 0 {
			remainingMinutes = 0
		}
		risk := remaining * arrival * sigma * math.Sqrt(remainingMinutes)

		slices[i] = model.Slice{
			Index:        i,
			ScheduledAt:  now.Add(step * time.Duration(i+1)),
			Quantity:     q,
			Price:        price,
			Slippage:     slip,
			SpreadCost:   halfSpread,
			Impact:       impact,
			ResidualRisk: risk,
			Status:       model.SlicePending,
		}
		// Only the surviving fraction of temporary impact carries forward.
		carryImpact = (carryImpact + impact) * (1 - p.DecayRate)
	}

	plan := &model.Plan{
		ID:         "",
		Symbol:     req.Symbol,
		Side:       req.Side,
		Quantity:   req.Quantity,
		Horizon:    model.DurationMS(horizon),
		Strategy:   req.Strategy,
		Params:     p,
		CreatedAt:  now,
		Status:     model.PlanActive,
		Slices:     slices,
		ArrivalMid: arrival,
	}
	return plan, nil
}

// WalkLevels accumulates depth from the best price and returns the
// quantity coverable and the volume-weighted average price needed to
// absorb qty. When the book covers less than qty, covered < qty and
// vwap covers only the available size.
func WalkLevels(levels []model.PriceLevel, qty float64) (covered float64, vwap float64) {
	need := qty
	notional := 0.0
	for _, l := range levels {
		if need <= 0 {
			break
		}
		take := l.Size
		if take > need {
			take = need
		}
		notional += take * l.Price
		covered += take
		need -= take
	}
	if covered > 0 {
		vwap = notional / covered
	}
	return covered, vwap
}

// sliceWeights returns per-slice fractions of the total quantity.
// TWAP/POV are uniform; VWAP follows a symmetric U-shaped intraday
// volume curve (heavier at the open and close of the horizon).
func sliceWeights(s model.Strategy, n int) []float64 {
	w := make([]float64, n)
	if n <= 0 {
		return w
	}
	switch s {
	case model.StrategyVWAP:
		// Distance of each slice centre from the horizon centre, in [0,1].
		sum := 0.0
		for i := 0; i < n; i++ {
			c := (float64(i) + 0.5) / float64(n)
			d := math.Abs(c-0.5) * 2 // 0 at centre, 1 at the ends
			w[i] = 1 + d
			sum += w[i]
		}
		for i := range w {
			w[i] /= sum
		}
		return w
	default: // TWAP and POV participate at a constant rate
		for i := range w {
			w[i] = 1 / float64(n)
		}
		return w
	}
}

// normalizeParams fills zero-valued parameters with documented defaults.
func normalizeParams(p model.Params) model.Params {
	if p.ImpactCoeff <= 0 {
		p.ImpactCoeff = DefaultImpactCoeff
	}
	if p.DecayRate < 0 || p.DecayRate >= 1 {
		p.DecayRate = DefaultDecayRate
	}
	if p.MaxSlices <= 0 {
		p.MaxSlices = DefaultMaxSlices
	}
	if p.RiskAnnual <= 0 {
		p.RiskAnnual = DefaultRiskAnnual
	}
	if p.Participate <= 0 || p.Participate > 1 {
		p.Participate = DefaultParticipate
	}
	return p
}

// rng is a deterministic xorshift64* uniform generator.
type rng struct {
	state uint64
}

func newRng(seed int64) *rng {
	s := uint64(seed)
	if s == 0 {
		s = 0x9E3779B97F4A7C15
	}
	return &rng{state: s}
}

// noise returns a small deterministic price perturbation of up to 1 bp
// of reference price; zero for RNGNone.
func (r *rng) noise(kind model.RNGKind, ref float64) float64 {
	if kind == model.RNGNone {
		return 0
	}
	// xorshift64*
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	u := float64((x*0x2545F4914F6CDD1D)>>11) / float64(1<<53) // [0,1)
	return (u - 0.5) * 2 * 0.0001 * ref
}
