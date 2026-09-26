// Package plan tracks in-flight execution plans: settlement of slices,
// mid-stream updates and cancellation.
package plan

import (
	"sync"
	"time"

	"github.com/guoyyit/execution-cost-estimator-go/model"
)

// Tracker stores plans by ID. Returned plans are always deep copies so
// callers can never mutate internal state field-by-field.
type Tracker struct {
	mu    sync.RWMutex
	plans map[string]*model.Plan
	now   func() time.Time
}

// NewTracker creates an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{plans: make(map[string]*model.Plan), now: time.Now}
}

// Put stores a plan (takes a copy).
func (t *Tracker) Put(p *model.Plan) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.plans[p.ID] = clonePlan(p)
}

// Get returns a deep copy of a plan.
func (t *Tracker) Get(id string) (*model.Plan, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	p, ok := t.plans[id]
	if !ok {
		return nil, false
	}
	return clonePlan(p), true
}

// Settle marks the slice at index settled. Already settled slices are
// idempotent. Settling a cancelled plan/slice is rejected.
func (t *Tracker) Settle(id string, index int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.plans[id]
	if !ok {
		return model.ErrPlanNotFound
	}
	if p.Status == model.PlanCancelled {
		return model.ErrPlanCancelled
	}
	if index < 0 || index >= len(p.Slices) {
		return model.ErrInvalidArgument
	}
	sl := &p.Slices[index]
	if sl.Status == model.SliceCancelled {
		return model.ErrPlanCancelled
	}
	if sl.Status != model.SliceSettled {
		sl.Status = model.SliceSettled
		sl.SettledAt = t.now()
	}
	t.refreshStatusLocked(p)
	return nil
}

// Cancel cancels a plan. All pending slices are marked cancelled and
// excluded from totals and residual risk; settled slices remain
// traceable. Idempotent on an already cancelled plan.
func (t *Tracker) Cancel(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.plans[id]
	if !ok {
		return model.ErrPlanNotFound
	}
	if p.Status == model.PlanCancelled {
		return nil
	}
	for i := range p.Slices {
		if p.Slices[i].Status == model.SlicePending {
			p.Slices[i].Status = model.SliceCancelled
		}
	}
	p.Status = model.PlanCancelled
	p.CancelledAt = t.now()
	return nil
}

// All returns deep copies of all plans.
func (t *Tracker) All() []*model.Plan {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*model.Plan, 0, len(t.plans))
	for _, p := range t.plans {
		out = append(out, clonePlan(p))
	}
	return out
}

// ReplaceAll is used during recovery.
func (t *Tracker) ReplaceAll(ps []*model.Plan) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.plans = make(map[string]*model.Plan, len(ps))
	for _, p := range ps {
		t.plans[p.ID] = clonePlan(p)
	}
}

// refreshStatusLocked completes a plan whose non-cancelled slices are
// all settled.
func (t *Tracker) refreshStatusLocked(p *model.Plan) {
	if p.Status == model.PlanCancelled {
		return
	}
	for i := range p.Slices {
		if p.Slices[i].Status == model.SlicePending {
			return
		}
	}
	p.Status = model.PlanCompleted
}

// clonePlan deep-copies a plan including its slice slice.
func clonePlan(p *model.Plan) *model.Plan {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Slices = append([]model.Slice(nil), p.Slices...)
	return &cp
}
