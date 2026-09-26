package plan

import (
	"testing"

	"github.com/guoyyit/execution-cost-estimator-go/model"
)

func mkPlan(id string, n int) *model.Plan {
	slices := make([]model.Slice, n)
	for i := range slices {
		slices[i] = model.Slice{
			Index: i, Quantity: 10, Price: 101,
			Slippage: 1.0, Impact: 0.2, ResidualRisk: float64(n-i) * 100,
			Status: model.SlicePending,
		}
	}
	return &model.Plan{ID: id, Symbol: "BTC", Status: model.PlanActive, Slices: slices, ArrivalMid: 100, Quantity: float64(n) * 10}
}

func TestSettleAndTotals(t *testing.T) {
	tr := NewTracker()
	tr.Put(mkPlan("p1", 3))
	if err := tr.Settle("p1", 0); err != nil {
		t.Fatal(err)
	}
	p, _ := tr.Get("p1")
	if p.FilledQuantity() != 10 {
		t.Fatalf("filled = %v", p.FilledQuantity())
	}
	if p.Status != model.PlanActive {
		t.Fatalf("status = %v", p.Status)
	}
	// Settle twice: idempotent.
	if err := tr.Settle("p1", 0); err != nil {
		t.Fatal(err)
	}
	_ = tr.Settle("p1", 1)
	_ = tr.Settle("p1", 2)
	p, _ = tr.Get("p1")
	if p.Status != model.PlanCompleted {
		t.Fatalf("status = %v, want completed", p.Status)
	}
}

func TestCancelExcludesPending(t *testing.T) {
	tr := NewTracker()
	tr.Put(mkPlan("p1", 4))
	if err := tr.Settle("p1", 0); err != nil {
		t.Fatal(err)
	}
	if err := tr.Cancel("p1"); err != nil {
		t.Fatal(err)
	}
	p, _ := tr.Get("p1")
	if p.Status != model.PlanCancelled {
		t.Fatalf("status = %v", p.Status)
	}
	// Only slice 0 (qty 10) counts.
	if p.TotalCost() != 10 {
		t.Fatalf("total cost = %v, want 10", p.TotalCost())
	}
	if p.FilledQuantity() != 10 {
		t.Fatalf("filled = %v, want 10", p.FilledQuantity())
	}
	// Residual risk comes from the last non-cancelled slice (slice 0).
	if p.ResidualRisk() != 400 {
		t.Fatalf("residual risk = %v, want 400", p.ResidualRisk())
	}
	for i := 1; i < 4; i++ {
		if p.Slices[i].Status != model.SliceCancelled {
			t.Fatalf("slice %d not cancelled", i)
		}
	}
	// No settlement after cancel.
	if err := tr.Settle("p1", 1); err != model.ErrPlanCancelled {
		t.Fatalf("want ErrPlanCancelled, got %v", err)
	}
	// Cancel is idempotent.
	if err := tr.Cancel("p1"); err != nil {
		t.Fatal(err)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	tr := NewTracker()
	tr.Put(mkPlan("p1", 2))
	p, _ := tr.Get("p1")
	p.Slices[0].Slippage = 999
	p2, _ := tr.Get("p1")
	if p2.Slices[0].Slippage == 999 {
		t.Fatal("returned plan must be a defensive copy")
	}
}

func TestSettleMissing(t *testing.T) {
	tr := NewTracker()
	if err := tr.Settle("nope", 0); err != model.ErrPlanNotFound {
		t.Fatalf("want not found, got %v", err)
	}
}
