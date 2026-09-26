package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/guoyyit/execution-cost-estimator-go/model"
)

func view() model.DepthView {
	return model.DepthView{
		Symbol: "BTC",
		Seq:    7,
		Bids:   []model.PriceLevel{{Price: 99.9, Size: 10}, {Price: 99.8, Size: 20}, {Price: 99.7, Size: 30}},
		Asks:   []model.PriceLevel{{Price: 100.1, Size: 10}, {Price: 100.2, Size: 20}, {Price: 100.3, Size: 30}},
	}
}

func baseReq(rng model.RNGKind) model.EstimateRequest {
	return model.EstimateRequest{
		Symbol:   "BTC",
		Side:     model.SideBuy,
		Quantity: 15,
		Horizon:  model.DurationMS(5 * time.Minute),
		Strategy: model.StrategyTWAP,
		Params: model.Params{
			ImpactCoeff: 0.1,
			DecayRate:   0.5,
			MaxSlices:   5,
			RiskAnnual:  0.4,
			Seed:        42,
			RNG:         rng,
		},
	}
}

func TestReproducibleSameSeed(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	p1, err := e.BuildPlan(baseReq(model.RNGUniform), view(), now)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := e.BuildPlan(baseReq(model.RNGUniform), view(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Slices) != len(p2.Slices) {
		t.Fatal("slice count differs")
	}
	for i := range p1.Slices {
		a, b := p1.Slices[i], p2.Slices[i]
		if a != b {
			t.Fatalf("slice %d differs:\n%+v\n%+v", i, a, b)
		}
	}
}

func TestDifferentSeedDifferentNoise(t *testing.T) {
	e := NewEngine()
	r1 := baseReq(model.RNGUniform)
	r2 := baseReq(model.RNGUniform)
	r2.Params.Seed = 43
	p1, _ := e.BuildPlan(r1, view(), time.Now())
	p2, _ := e.BuildPlan(r2, view(), time.Now())
	same := true
	for i := range p1.Slices {
		if p1.Slices[i].Price != p2.Slices[i].Price {
			same = false
		}
	}
	if same {
		t.Fatal("different seeds produced identical prices")
	}
}

func TestNoRNGDeterministicBase(t *testing.T) {
	e := NewEngine()
	r := baseReq(model.RNGNone)
	p, err := e.BuildPlan(r, view(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// First buy slice: best ask 100.1 + half spread 0.1 + impact, no
	// carry impact yet.
	q := 15.0 / 5
	adv := 60.0
	wantImpact := 0.1 * (q / adv)
	wantPrice := 100.1 + 0.1 + wantImpact
	if math.Abs(p.Slices[0].Price-wantPrice) > 1e-12 {
		t.Fatalf("price = %v, want %v", p.Slices[0].Price, wantPrice)
	}
	if p.Slices[0].Impact != wantImpact {
		t.Fatalf("impact = %v, want %v", p.Slices[0].Impact, wantImpact)
	}
	if math.Abs(p.ArrivalMid-100.0) > 1e-12 {
		t.Fatalf("mid = %v", p.ArrivalMid)
	}
	// Total quantity conserved, last slice absorbs rounding.
	var total float64
	for _, s := range p.Slices {
		total += s.Quantity
	}
	if math.Abs(total-15) > 1e-9 {
		t.Fatalf("quantity not conserved: %v", total)
	}
	// Residual risk declines to ~0 on the final slice.
	last := p.Slices[len(p.Slices)-1]
	if math.Abs(last.ResidualRisk) > 1e-9 {
		t.Fatalf("final residual risk = %v, want 0", last.ResidualRisk)
	}
}

func TestVWAPWeightsAreUShape(t *testing.T) {
	w := sliceWeights(model.StrategyVWAP, 5)
	sum := 0.0
	for _, x := range w {
		sum += x
	}
	if math.Abs(sum-1) > 1e-12 {
		t.Fatalf("weights sum = %v", sum)
	}
	if !(w[0] > w[2] && w[4] > w[2]) {
		t.Fatalf("VWAP weights not U-shaped: %v", w)
	}
}

func TestPOVParticipationCap(t *testing.T) {
	e := NewEngine()
	r := baseReq(model.RNGNone)
	r.Strategy = model.StrategyPOV
	r.Params.Participate = 0.1 // cap = 0.1*60 = 6 per slice => 15 needs 3
	r.Params.MaxSlices = 10
	p, err := e.BuildPlan(r, view(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Slices) != 3 {
		t.Fatalf("POV slices = %d, want 3", len(p.Slices))
	}
}

func TestSellSidePricesBelowMid(t *testing.T) {
	e := NewEngine()
	r := baseReq(model.RNGNone)
	r.Side = model.SideSell
	p, err := e.BuildPlan(r, view(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Slices {
		if s.Price >= p.ArrivalMid {
			t.Fatalf("sell slice price %v >= mid %v", s.Price, p.ArrivalMid)
		}
		if s.Slippage < 0 {
			t.Fatalf("slippage must be positive cost, got %v", s.Slippage)
		}
	}
}

func TestRejectsBadInput(t *testing.T) {
	e := NewEngine()
	r := baseReq(model.RNGNone)
	r.Quantity = 0
	if _, err := e.BuildPlan(r, view(), time.Now()); err != model.ErrInvalidArgument {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	empty := model.DepthView{Symbol: "BTC"}
	if _, err := e.BuildPlan(baseReq(model.RNGNone), empty, time.Now()); err != model.ErrEmptyDepth {
		t.Fatalf("want ErrEmptyDepth, got %v", err)
	}
}

func TestWalkLevels(t *testing.T) {
	levels := []model.PriceLevel{{Price: 100, Size: 5}, {Price: 101, Size: 5}}
	c, vwap := WalkLevels(levels, 8)
	if c != 8 {
		t.Fatalf("covered = %v", c)
	}
	if math.Abs(vwap-(5.0*100+3.0*101)/8.0) > 1e-9 {
		t.Fatalf("vwap = %v", vwap)
	}
	c, _ = WalkLevels(levels, 100)
	if c != 10 {
		t.Fatalf("partial cover = %v, want 10", c)
	}
}
