package estimator

import (
	"context"
	"testing"
	"time"
)

func TestSliceWeightsSumToOneAndShape(t *testing.T) {
	m := newImpactModel(testCfg())
	for _, st := range []Strategy{TWAP, VWAP, IS} {
		w := m.sliceWeights(st, 8)
		sum := 0.0
		for _, x := range w {
			sum += x
			if x <= 0 {
				t.Fatalf("%s has non-positive weight %v", st, x)
			}
		}
		if abs(sum-1) > 1e-12 {
			t.Fatalf("%s weights sum %v", st, sum)
		}
		switch st {
		case TWAP:
			for i := 1; i < len(w); i++ {
				if w[i] != w[0] {
					t.Fatal("TWAP must be equal-weight")
				}
			}
		case IS:
			for i := 1; i < len(w); i++ {
				if w[i] >= w[i-1] {
					t.Fatal("IS must front-load (strictly decreasing)")
				}
			}
		}
	}
}

func TestSeedChangesOnlyNoiseDeterministically(t *testing.T) {
	s := newTestService(t, nil)
	snap := seedBook(t, s)
	ctx := context.Background()
	req := buyReq(4)
	r1, err := s.Estimate(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req2 := req
	req2.Seed = 42
	r2, _ := s.Estimate(ctx, req2)
	// Default cfg seed is 42, so explicit 42 must match exactly.
	if r1.TotalCost != r2.TotalCost {
		t.Fatalf("explicit seed must equal configured seed: %v vs %v", r1.TotalCost, r2.TotalCost)
	}
	req3 := req
	req3.Seed = 7
	r3, _ := s.Estimate(ctx, req3)
	if r3.TotalCost == r1.TotalCost {
		t.Fatal("different seed should perturb temporary impact noise")
	}
	// Slippage (book-walk) component is unaffected by seed.
	for i := range r1.Slices {
		if r1.Slices[i].SlippageCost != r3.Slices[i].SlippageCost {
			t.Fatal("seed must not change the deterministic book-walk slippage")
		}
	}
	if snap.Mid != 100 {
		t.Fatalf("mid = %v", snap.Mid)
	}
}

func TestSellSideWalksBids(t *testing.T) {
	s := newTestService(t, nil)
	seedBook(t, s)
	req := EstimateRequest{Symbol: "BTC-USD", Side: Sell, Quantity: 4, Horizon: 10 * time.Minute, Strategy: TWAP}
	r, err := s.Estimate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, sl := range r.Slices {
		if sl.FillPrice > sl.Mid {
			t.Fatalf("sell must fill at or below mid, got %v > %v", sl.FillPrice, sl.Mid)
		}
	}
}

func TestSettleAllCompletesPlan(t *testing.T) {
	s := newTestService(t, nil)
	seedBook(t, s)
	ctx := context.Background()
	p, err := s.CreatePlan(ctx, buyReq(4))
	if err != nil {
		t.Fatal(err)
	}
	for i := range p.Slices {
		if err := s.Settle(ctx, p.ID, i, 1.0); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}
	r, _ := s.GetPlan(ctx, p.ID)
	if r.Status != PlanCompleted {
		t.Fatalf("want COMPLETED, got %s", r.Status)
	}
	if abs(r.TotalCost-float64(len(p.Slices))) > 1e-9 {
		t.Fatalf("total must be sum of realized costs, got %v", r.TotalCost)
	}
	if r.ProjectedRemainingCost != 0 || r.RemainingRisk != 0 {
		t.Fatal("completed plan must have no remaining projection/risk")
	}
}

func TestDoubleSettleRejected(t *testing.T) {
	s := newTestService(t, nil)
	seedBook(t, s)
	ctx := context.Background()
	p, _ := s.CreatePlan(ctx, buyReq(4))
	if err := s.Settle(ctx, p.ID, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, p.ID, 0, 2); err == nil {
		t.Fatal("double settle must be rejected")
	}
	r, _ := s.GetPlan(ctx, p.ID)
	if r.Slices[0].RealizedCost != 1 {
		t.Fatal("settled slice must be immutable")
	}
}
