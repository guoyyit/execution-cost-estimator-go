package estimator

import (
	"time"
)

// plan is an execution plan: a projected slice schedule plus settled reality.
type plan struct {
	id        string
	symbol    string
	side      Side
	strategy  Strategy
	totalQty  float64
	horizon   time.Duration
	status    PlanStatus
	slices    []SliceReport
	baseSeq   uint64
	seed      int64
	weights   []float64
	createdAt time.Time
	updatedAt time.Time
}

func newPlan(id string, req EstimateRequest, baseSeq uint64, seed int64,
	weights []float64, slices []SliceReport, now time.Time) *plan {
	return &plan{
		id:        id,
		symbol:    req.Symbol,
		side:      req.Side,
		strategy:  req.Strategy,
		totalQty:  req.Quantity,
		horizon:   req.Horizon,
		status:    PlanRunning,
		slices:    slices,
		baseSeq:   baseSeq,
		seed:      seed,
		weights:   weights,
		createdAt: now,
		updatedAt: now,
	}
}

// report builds a consistent, fully copied view of the plan.
func (p *plan) report() PlanReport {
	slices := append([]SliceReport(nil), p.slices...)
	r := PlanReport{
		ID:            p.id,
		Symbol:        p.symbol,
		Side:          p.side,
		Strategy:      p.strategy,
		TotalQuantity: p.totalQty,
		Horizon:       p.horizon,
		Status:        p.status,
		Slices:        slices,
		BaseDepthSeq:  p.baseSeq,
		CreatedAt:     p.createdAt,
		UpdatedAt:     p.updatedAt,
	}
	for _, s := range slices {
		switch s.Status {
		case SliceSettled:
			r.SettledCost += s.RealizedCost
			r.TotalCost += s.RealizedCost
		case SlicePending:
			r.ProjectedRemainingCost += s.TotalCost
			r.TotalCost += s.TotalCost
			r.RemainingRisk += s.ResidualRisk
		}
	}
	return r
}

func (p *plan) settledCount() int {
	n := 0
	for _, s := range p.slices {
		if s.Status == SliceSettled {
			n++
		}
	}
	return n
}

func (p *plan) settledQty() float64 {
	q := 0.0
	for _, s := range p.slices {
		if s.Status == SliceSettled {
			q += s.Quantity
		}
	}
	return q
}

// settle marks slice idx as executed at the given realized cost. Settled
// slices are immutable and their cost is traceable forever.
func (p *plan) settle(idx int, realizedCost float64, now time.Time) error {
	if p.status != PlanRunning {
		return ErrPlanClosed
	}
	if idx < 0 || idx >= len(p.slices) {
		return ErrInvalidArgument
	}
	s := &p.slices[idx]
	if s.Status == SliceSettled {
		return ErrSliceSettled
	}
	if s.Status == SliceCanceled {
		return ErrPlanClosed
	}
	s.Status = SliceSettled
	s.RealizedCost = realizedCost
	// Settled reality replaces the projection for traceable accounting.
	s.TotalCost = s.SlippageCost + s.ImpactCost
	p.updatedAt = now
	if p.settledCount() == len(p.slices) {
		p.status = PlanCompleted
	}
	return nil
}

// update mutates a running plan. Settled slices are kept verbatim and only
// the remaining quantity is re-projected; canceled plans cannot be updated.
func (p *plan) update(totalQty float64, horizon time.Duration, reproject ReprojectFn, now time.Time) error {
	if p.status != PlanRunning {
		return ErrPlanClosed
	}
	settled := p.settledQty()
	if totalQty < settled {
		return ErrInvalidArgument
	}
	remaining := totalQty - settled
	newPending, err := reproject(p.side, remaining, horizon)
	if err != nil {
		return err
	}
	kept := make([]SliceReport, 0, p.settledCount()+len(newPending))
	var baseOffset time.Duration
	for _, s := range p.slices {
		if s.Status == SliceSettled {
			kept = append(kept, s)
			baseOffset = s.Offset
		}
	}
	// New schedule starts one slice after the last settled slice; offsets are
	// shifted so the timeline stays monotonic.
	span := horizon / time.Duration(max(len(newPending), 1))
	start := baseOffset + span
	for k, s := range newPending {
		s.Index = len(kept) + k
		s.Offset = start + time.Duration(k)*span
		kept = append(kept, s)
	}
	p.slices = kept
	p.totalQty = totalQty
	p.horizon = horizon
	p.updatedAt = now
	if p.settledCount() == len(p.slices) {
		p.status = PlanCompleted
	}
	return nil
}

// cancel stops a running plan. Remaining pending slices are marked CANCELED
// and excluded from total cost and residual risk in every later report.
func (p *plan) cancel(now time.Time) error {
	if p.status != PlanRunning {
		return ErrPlanClosed
	}
	for i := range p.slices {
		if p.slices[i].Status == SlicePending {
			p.slices[i].Status = SliceCanceled
			p.slices[i].ResidualRisk = 0
		}
	}
	p.status = PlanCanceled
	p.updatedAt = now
	return nil
}

// ReprojectFn re-projects a remaining quantity into fresh slice reports.
type ReprojectFn func(side Side, remainingQty float64, horizon time.Duration) ([]SliceReport, error)

// ---- persistence conversions ----

func (p *plan) toPersisted() persistedPlan {
	return persistedPlan{
		ID:        p.id,
		Symbol:    p.symbol,
		Side:      p.side,
		Strategy:  p.strategy,
		TotalQty:  p.totalQty,
		Horizon:   p.horizon,
		Status:    p.status,
		Slices:    append([]SliceReport(nil), p.slices...),
		BaseSeq:   p.baseSeq,
		Seed:      p.seed,
		Weights:   append([]float64(nil), p.weights...),
		CreatedAt: p.createdAt,
		UpdatedAt: p.updatedAt,
	}
}

func planFromPersisted(pd persistedPlan) *plan {
	return &plan{
		id:        pd.ID,
		symbol:    pd.Symbol,
		side:      pd.Side,
		strategy:  pd.Strategy,
		totalQty:  pd.TotalQty,
		horizon:   pd.Horizon,
		status:    pd.Status,
		slices:    append([]SliceReport(nil), pd.Slices...),
		baseSeq:   pd.BaseSeq,
		seed:      pd.Seed,
		weights:   append([]float64(nil), pd.Weights...),
		createdAt: pd.CreatedAt,
		updatedAt: pd.UpdatedAt,
	}
}
