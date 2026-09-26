package estimator

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testCfg() Config {
	return Config{
		Symbol:          "BTC-USD",
		ImpactCoeff:     0.1,
		ImpactExponent:  0.5,
		DecayRate:       0.3,
		MaxSlices:       12,
		MinSliceSeconds: 60,
		Volatility:      0.02,
		RiskAversion:    1.5,
		Seed:            42,
	}
}

func newTestService(t *testing.T, store Store) *Service {
	t.Helper()
	s, err := NewService(testCfg(), store)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

func seedBook(t *testing.T, s *Service) DepthSnapshot {
	t.Helper()
	ctx := context.Background()
	err := s.ApplyDepth(ctx, sampleSnapshot(1))
	if err != nil {
		t.Fatalf("ApplyDepth: %v", err)
	}
	snap, err := s.DepthSnapshot(ctx)
	if err != nil {
		t.Fatalf("DepthSnapshot: %v", err)
	}
	return snap
}

func buyReq(qty float64) EstimateRequest {
	return EstimateRequest{Symbol: "BTC-USD", Side: Buy, Quantity: qty, Horizon: 10 * time.Minute, Strategy: TWAP}
}

func TestEstimateReproducibleSameSeed(t *testing.T) {
	s := newTestService(t, nil)
	seedBook(t, s)
	ctx := context.Background()
	r1, err := s.Estimate(ctx, buyReq(4))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Estimate(ctx, buyReq(4))
	if err != nil {
		t.Fatal(err)
	}
	if r1.BaseDepthSeq != r2.BaseDepthSeq || len(r1.Slices) != len(r2.Slices) {
		t.Fatal("estimate structure must be identical")
	}
	for i := range r1.Slices {
		a, b := r1.Slices[i], r2.Slices[i]
		if a != b {
			t.Fatalf("slice %d differs: %+v vs %+v", i, a, b)
		}
	}
	// A different seed may differ; same explicit seed on another service too.
	s2 := newTestService(t, nil)
	seedBook(t, s2)
	r3, err := s2.Estimate(ctx, buyReq(4))
	if err != nil {
		t.Fatal(err)
	}
	if r3.TotalCost != r1.TotalCost {
		t.Fatalf("fresh service with same seed must reproduce cost, %v vs %v", r3.TotalCost, r1.TotalCost)
	}
}

func TestEstimateRejectsBeforeDepth(t *testing.T) {
	s := newTestService(t, nil)
	_, err := s.Estimate(context.Background(), buyReq(1))
	if !errors.Is(err, ErrDepthUnavailable) {
		t.Fatalf("want ErrDepthUnavailable, got %v", err)
	}
}

func TestEstimateRejectsInsufficientDepth(t *testing.T) {
	s := newTestService(t, nil)
	seedBook(t, s)
	_, err := s.Estimate(context.Background(), buyReq(200))
	if !errors.Is(err, ErrDepthInsufficient) {
		t.Fatalf("want ErrDepthInsufficient, got %v", err)
	}
}

func TestConcurrentEstimateAndUpdatesUseConsistentSnapshots(t *testing.T) {
	s := newTestService(t, nil)
	seedBook(t, s)
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: alternating snapshots keep the book always complete; increments
	// change sizes. All snapshots carry the same symbol.
	wg.Add(1)
	go func() {
		defer wg.Done()
		seq := uint64(1)
		k := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			seq++
			bidPx := 99.0
			askPx := 101.0
			k++
			upd := DepthUpdate{
				Symbol: "BTC-USD",
				Seq:    seq,
				Bids:   mkLevels([2]float64{bidPx, float64(50 + k%50)}),
				Asks:   mkLevels([2]float64{askPx, float64(50 + k%50)}),
			}
			if err := s.ApplyDepth(ctx, upd); err != nil {
				t.Errorf("ApplyDepth: %v", err)
				return
			}
			time.Sleep(time.Microsecond * 100)
		}
	}()

	// Readers: each estimate must be internally consistent: every slice's mid
	// must match the report base snapshot and seq must be a known one.
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Estimate(ctx, buyReq(5))
			if err != nil {
				t.Errorf("estimate: %v", err)
				return
			}
			for _, sl := range r.Slices {
				if sl.Mid != 100 {
					t.Errorf("mixed snapshot observed: mid %v in report seq %d", sl.Mid, r.BaseDepthSeq)
					return
				}
				if sl.FillPrice < sl.Mid {
					t.Errorf("buy fill below mid: %v < %v", sl.FillPrice, sl.Mid)
					return
				}
			}
		}()
	}
	// Let readers finish, then stop the writer.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	close(stop)
	wg.Wait()
}

func TestPlanCancelExcludesRemaining(t *testing.T) {
	s := newTestService(t, nil)
	seedBook(t, s)
	ctx := context.Background()
	plan, err := s.CreatePlan(ctx, buyReq(4))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Slices) < 3 {
		t.Fatalf("want several slices, got %d", len(plan.Slices))
	}
	if err := s.Settle(ctx, plan.ID, 0, 1.5); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelPlan(ctx, plan.ID); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != PlanCanceled {
		t.Fatalf("want CANCELED, got %s", r.Status)
	}
	for i, sl := range r.Slices {
		switch {
		case i == 0 && sl.Status != SliceSettled:
			t.Fatalf("slice 0 must remain settled, got %s", sl.Status)
		case i > 0 && sl.Status != SliceCanceled:
			t.Fatalf("slice %d must be canceled, got %s", i, sl.Status)
		}
	}
	if r.SettledCost != 1.5 {
		t.Fatalf("settled cost traceable, got %v", r.SettledCost)
	}
	if r.ProjectedRemainingCost != 0 || r.RemainingRisk != 0 {
		t.Fatalf("canceled slices must not count, got cost %v risk %v", r.ProjectedRemainingCost, r.RemainingRisk)
	}
	if r.TotalCost != 1.5 {
		t.Fatalf("total must equal settled only, got %v", r.TotalCost)
	}
	// Further settlement is rejected.
	if err := s.Settle(ctx, plan.ID, 1, 1); !errors.Is(err, ErrPlanClosed) {
		t.Fatalf("want ErrPlanClosed, got %v", err)
	}
	// Double cancel rejected.
	if err := s.CancelPlan(ctx, plan.ID); !errors.Is(err, ErrPlanClosed) {
		t.Fatalf("want ErrPlanClosed, got %v", err)
	}
}

func TestPlanUpdateKeepsSettled(t *testing.T) {
	s := newTestService(t, nil)
	seedBook(t, s)
	ctx := context.Background()
	plan, err := s.CreatePlan(ctx, buyReq(4))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, plan.ID, 0, 2.0); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePlan(ctx, plan.ID, 8, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Slices[0].Status != SliceSettled || r.Slices[0].RealizedCost != 2.0 {
		t.Fatal("settled slice must remain immutable")
	}
	var qty float64
	for _, sl := range r.Slices {
		qty += sl.Quantity
	}
	if abs(qty-8) > 1e-9 {
		t.Fatalf("updated plan quantity must total 8, got %v", qty)
	}
	if r.Status != PlanRunning {
		t.Fatal("plan should remain running")
	}
	// Cannot shrink below settled quantity.
	if err := s.UpdatePlan(ctx, plan.ID, 0.1, 10*time.Minute); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}

func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s1 := newTestService(t, NewFileStore(path))
	seedBook(t, s1)
	ctx := context.Background()
	plan, err := s1.CreatePlan(ctx, buyReq(4))
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Settle(ctx, plan.ID, 0, 2.5); err != nil {
		t.Fatal(err)
	}
	if err := s1.CancelPlan(ctx, plan.ID); err != nil {
		t.Fatal(err)
	}

	// Simulate restart from the same file.
	s2 := newTestService(t, NewFileStore(path))
	r, err := s2.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("plan must survive restart: %v", err)
	}
	if r.Status != PlanCanceled || r.SettledCost != 2.5 {
		t.Fatalf("progress not restored: %+v", r)
	}
	if r.Slices[0].Status != SliceSettled {
		t.Fatal("settled slice not restored")
	}
	// Depth recovers at its committed seq.
	if s2.book.lastSeq != 1 || !s2.book.complete() {
		t.Fatalf("depth not restored, seq=%d complete=%v", s2.book.lastSeq, s2.book.complete())
	}
	// Late/duplicate pre-restart updates must be ignored.
	if err := s2.ApplyDepth(ctx, sampleSnapshot(1)); err != nil {
		t.Fatal(err)
	}
	if s2.book.lastSeq != 1 {
		t.Fatalf("duplicate seq must not move book, got %d", s2.book.lastSeq)
	}
	// A newer increment applies and estimation works after restart.
	err = s2.ApplyDepth(ctx, DepthUpdate{
		Symbol: "BTC-USD", Seq: 2,
		Bids: mkLevels([2]float64{99, 5}, [2]float64{98, 8}),
		Asks: mkLevels([2]float64{101, 5}, [2]float64{102, 8}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s2.book.lastSeq != 2 {
		t.Fatalf("seq must advance to 2, got %d", s2.book.lastSeq)
	}
	if _, err := s2.Estimate(ctx, buyReq(4)); err != nil {
		t.Fatalf("estimate after recovery: %v", err)
	}
}

func TestRestartReproducibilityAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s1 := newTestService(t, NewFileStore(path))
	seedBook(t, s1)
	plan1, err := s1.CreatePlan(context.Background(), buyReq(4))
	if err != nil {
		t.Fatal(err)
	}
	s2 := newTestService(t, NewFileStore(path))
	plan2, err := s2.GetPlan(context.Background(), plan1.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range plan1.Slices {
		if plan1.Slices[i] != plan2.Slices[i] {
			t.Fatalf("slice %d drifted across restart", i)
		}
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestPlanUpdateDownToSettledCompletes(t *testing.T) {
	s := newTestService(t, nil)
	seedBook(t, s)
	ctx := context.Background()
	plan, err := s.CreatePlan(ctx, buyReq(4))
	if err != nil {
		t.Fatal(err)
	}
	first := plan.Slices[0].Quantity
	if err := s.Settle(ctx, plan.ID, 0, 1.0); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePlan(ctx, plan.ID, first, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != PlanCompleted {
		t.Fatalf("plan fully settled after shrink must complete, got %s", r.Status)
	}
	if abs(r.TotalQuantity-first) > 1e-9 || len(r.Slices) != 1 {
		t.Fatalf("only the settled slice should remain, qty=%v slices=%d", r.TotalQuantity, len(r.Slices))
	}
}
