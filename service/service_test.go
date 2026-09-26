package service

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/guoyyit/execution-cost-estimator-go/depth"
	"github.com/guoyyit/execution-cost-estimator-go/model"
)

func bigSnapshot(seq int64) model.Snapshot {
	bids, asks := []model.PriceLevel{}, []model.PriceLevel{}
	for i := 0; i < 20; i++ {
		bids = append(bids, model.PriceLevel{Price: 100 - float64(i)*0.1 - 0.05, Size: 50})
		asks = append(asks, model.PriceLevel{Price: 100 + float64(i)*0.1 + 0.05, Size: 50})
	}
	return model.Snapshot{View: model.DepthView{Symbol: "BTC", Seq: seq, Bids: bids, Asks: asks}}
}

func newTestSvc(t *testing.T, path string) *Service {
	t.Helper()
	s := New(Config{PersistPath: path, Now: func() time.Time {
		return time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	}})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func req() model.EstimateRequest {
	return model.EstimateRequest{
		Symbol: "BTC", Side: model.SideBuy, Quantity: 50,
		Horizon: model.DurationMS(5 * time.Minute), Strategy: model.StrategyTWAP,
		Params: model.Params{ImpactCoeff: 0.1, DecayRate: 0.5, MaxSlices: 5, RiskAnnual: 0.4, Seed: 7, RNG: model.RNGUniform},
	}
}

func TestEstimateEmptyDepthRejected(t *testing.T) {
	s := newTestSvc(t, filepath.Join(t.TempDir(), "s.json"))
	if _, err := s.Estimate(req()); !errors.Is(err, model.ErrEmptyDepth) {
		t.Fatalf("want ErrEmptyDepth, got %v", err)
	}
}

func TestEstimateStaleDepthRejected(t *testing.T) {
	s := newTestSvc(t, filepath.Join(t.TempDir(), "s.json"))
	if err := s.HandleSnapshot(bigSnapshot(1)); err != nil {
		t.Fatal(err)
	}
	_ = s.HandleUpdate(model.DepthUpdate{Symbol: "BTC", Seq: 3}) // gap
	if _, err := s.Estimate(req()); !errors.Is(err, depth.ErrStaleDepth) {
		t.Fatalf("want stale rejection, got %v", err)
	}
}

func TestInsufficientDepthRejectOrDegrade(t *testing.T) {
	s := newTestSvc(t, filepath.Join(t.TempDir(), "s.json"))
	thin := model.Snapshot{View: model.DepthView{
		Symbol: "BTC", Seq: 1,
		Bids: []model.PriceLevel{{Price: 99.9, Size: 2}},
		Asks: []model.PriceLevel{{Price: 100.1, Size: 2}},
	}}
	if err := s.HandleSnapshot(thin); err != nil {
		t.Fatal(err)
	}
	r := req()
	r.Quantity = 100
	r.RejectIfInsufficient = true
	if _, err := s.Estimate(r); !errors.Is(err, model.ErrInsufficientDepth) {
		t.Fatalf("want ErrInsufficientDepth, got %v", err)
	}
	r.RejectIfInsufficient = false
	rep, err := s.Estimate(r)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Degraded == nil || rep.Degraded.Available != 2 {
		t.Fatalf("want explicit degraded notice, got %+v", rep.Degraded)
	}
	if rep.Degraded.Requested != 100 {
		t.Fatalf("requested = %v", rep.Degraded.Requested)
	}
}

func TestEstimateHappyAndReproducible(t *testing.T) {
	s := newTestSvc(t, filepath.Join(t.TempDir(), "s.json"))
	if err := s.HandleSnapshot(bigSnapshot(4)); err != nil {
		t.Fatal(err)
	}
	r1 := req()
	a, err := s.Estimate(r1)
	if err != nil {
		t.Fatal(err)
	}
	if a.ViewSeq != 4 || len(a.Slices) != 5 {
		t.Fatalf("report wrong: %+v", a)
	}
	if a.Degraded != nil {
		t.Fatalf("deep book should not be degraded")
	}
	// Same inputs, same seed -> identical numbers on a fresh svc.
	s2 := newTestSvc(t, filepath.Join(t.TempDir(), "s2.json"))
	_ = s2.HandleSnapshot(bigSnapshot(4))
	b, err := s2.Estimate(req())
	if err != nil {
		t.Fatal(err)
	}
	for i := range a.Slices {
		if a.Slices[i] != b.Slices[i] {
			t.Fatalf("slice %d not reproducible:\n%+v\n%+v", i, a.Slices[i], b.Slices[i])
		}
	}
}

func TestSettleCancelIntegration(t *testing.T) {
	s := newTestSvc(t, filepath.Join(t.TempDir(), "s.json"))
	_ = s.HandleSnapshot(bigSnapshot(1))
	rep, err := s.Estimate(req())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Settle(rep.PlanID, 0); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.Cancel(rep.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.ResidualRisk() != rep.Slices[0].ResidualRisk {
		t.Fatalf("risk after cancel = %v, want %v", cancelled.ResidualRisk(), rep.Slices[0].ResidualRisk)
	}
	if cancelled.TotalCost() != rep.Slices[0].Quantity*rep.Slices[0].Slippage {
		t.Fatalf("cost after cancel = %v", cancelled.TotalCost())
	}
	if cancelled.FilledQuantity() != rep.Slices[0].Quantity {
		t.Fatalf("filled = %v", cancelled.FilledQuantity())
	}
	if cancelled.Slices[1].Status != model.SliceCancelled {
		t.Fatal("later slice not cancelled")
	}
	// Cancelled slices cannot be settled.
	if _, err := s.Settle(rep.PlanID, 2); !errors.Is(err, model.ErrPlanCancelled) {
		t.Fatalf("want ErrPlanCancelled, got %v", err)
	}
}

func TestConcurrentEstimateAndUpdates(t *testing.T) {
	s := newTestSvc(t, filepath.Join(t.TempDir(), "s.json"))
	_ = s.HandleSnapshot(bigSnapshot(1))

	var wg sync.WaitGroup
	// Writer: contiguous updates keep the book fresh and non-stale.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for seq := int64(2); seq <= 300; seq++ {
			// Small harmless update (delete/add nothing): zero-level
			// updates just advance the sequence.
			_ = s.HandleUpdate(model.DepthUpdate{
				Symbol: "BTC", Seq: seq,
				Asks: []model.PriceLevel{{Price: 200 + float64(seq), Size: 1}},
			})
		}
	}()
	// Readers: every successful estimate must be internally consistent:
	// report seq matches a real view and slices sum to the quantity.
	seenSeqs := map[int64]bool{}
	var mu sync.Mutex
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				rep, err := s.Estimate(req())
				if err != nil {
					if !errors.Is(err, depth.ErrStaleDepth) {
						t.Errorf("unexpected error: %v", err)
					}
					continue
				}
				var sum float64
				for _, sl := range rep.Slices {
					sum += sl.Quantity
				}
				if d := sum - 50.0; d > 1e-9 || d < -1e-9 {
					t.Errorf("mixed read: quantity sum %v", sum)
				}
				mu.Lock()
				seenSeqs[rep.ViewSeq] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seenSeqs) < 2 {
		t.Fatalf("expected estimates at multiple consistent seqs, got %v", seenSeqs)
	}
}

func TestRestartRecovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state.json")
	s := newTestSvc(t, dir)
	_ = s.HandleSnapshot(bigSnapshot(10))
	rep, err := s.Estimate(req())
	if err != nil {
		t.Fatal(err)
	}
	planID := rep.PlanID
	if _, err := s.Settle(planID, 0); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// New process, same file: plans and progress come back; the book is
	// empty until a fresh snapshot.
	s2 := New(Config{PersistPath: dir, Now: func() time.Time {
		return time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	}})
	if err := s2.Recover(); err != nil {
		t.Fatal(err)
	}
	p, err := s2.GetPlan(planID)
	if err != nil {
		t.Fatalf("plan lost across restart: %v", err)
	}
	if p.Slices[0].Status != model.SliceSettled || p.Slices[1].Status != model.SlicePending {
		t.Fatalf("progress not recovered: %+v", p.Slices)
	}
	if p.Status != model.PlanActive {
		t.Fatalf("unfinished plan must remain active, got %v", p.Status)
	}
	// No depth yet: estimates are refused, never zero-depth.
	if _, err := s2.Estimate(req()); !errors.Is(err, model.ErrEmptyDepth) {
		t.Fatalf("want empty depth after restart, got %v", err)
	}
	// Late / duplicate replay of old updates is dropped; no drift.
	_ = s2.HandleUpdate(model.DepthUpdate{Symbol: "BTC", Seq: 5})
	_ = s2.HandleUpdate(model.DepthUpdate{Symbol: "BTC", Seq: 10})
	if _, err := s2.Estimate(req()); !errors.Is(err, model.ErrEmptyDepth) {
		t.Fatalf("replayed old updates must not fabricate depth, got %v", err)
	}
	// A fresh snapshot resumes normal operation; a future update already
	// buffered replays; result identical to pre-restart estimate.
	_ = s2.HandleUpdate(model.DepthUpdate{Symbol: "BTC", Seq: 12,
		Asks: []model.PriceLevel{{Price: 201, Size: 1}}})
	if err := s2.HandleSnapshot(bigSnapshot(11)); err != nil {
		t.Fatal(err)
	}
	st := s2.book.StateOf("BTC")
	if st.LastSeq != 12 {
		t.Fatalf("seq = %d, want 12", st.LastSeq)
	}
	// Continue settling the recovered plan.
	if _, err := s2.Settle(planID, 1); err != nil {
		t.Fatalf("settle after recovery: %v", err)
	}
}

func TestCancelPersistsAcrossRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state.json")
	s := newTestSvc(t, dir)
	_ = s.HandleSnapshot(bigSnapshot(1))
	rep, _ := s.Estimate(req())
	_, _ = s.Cancel(rep.PlanID)
	_ = s.Close()

	s2 := New(Config{PersistPath: dir})
	if err := s2.Recover(); err != nil {
		t.Fatal(err)
	}
	p, err := s2.GetPlan(rep.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != model.PlanCancelled {
		t.Fatalf("cancel lost across restart: %v", p.Status)
	}
	cost := p.TotalCost()
	risk := p.ResidualRisk()
	if cost != 0 || risk != 0 {
		t.Fatalf("cancelled-before-any-slice plan must have zero cost/risk, got %v/%v", cost, risk)
	}
}

func TestGetPlanNotFound(t *testing.T) {
	s := newTestSvc(t, filepath.Join(t.TempDir(), "s.json"))
	if _, err := s.GetPlan("missing"); !errors.Is(err, model.ErrPlanNotFound) {
		t.Fatalf("got %v", err)
	}
}

var _ = fmt.Sprintf
