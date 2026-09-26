// Package service wires depth, strategy, plan tracking and persistence
// into a thread-safe execution cost estimation service.
//
// Every estimation reads one complete DepthView under the book's read
// lock, so it can never observe a field-level mixed intermediate state.
// Empty or stale depth is rejected; insufficient depth is either
// rejected (RejectIfInsufficient) or answered with an explicitly
// Degraded report.
package service

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guoyyit/execution-cost-estimator-go/depth"
	"github.com/guoyyit/execution-cost-estimator-go/model"
	"github.com/guoyyit/execution-cost-estimator-go/persist"
	"github.com/guoyyit/execution-cost-estimator-go/plan"
	"github.com/guoyyit/execution-cost-estimator-go/strategy"
)

// Service is the cost estimation facade.
type Service struct {
	mu      sync.Mutex // serialises commits (plan creation / settle / cancel)
	book    *depth.Book
	engine  *strategy.Engine
	tracker *plan.Tracker
	store   *persist.Store
	now     func() time.Time
	idSeq   atomic.Uint64
}

// Config configures a Service.
type Config struct {
	PersistPath string
	Now         func() time.Time
}

// New builds a Service with empty in-memory state.
func New(cfg Config) *Service {
	s := &Service{
		book:    depth.NewBook(),
		engine:  strategy.NewEngine(),
		tracker: plan.NewTracker(),
		now:     cfg.Now,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if cfg.PersistPath != "" {
		s.store = persist.NewStore(cfg.PersistPath)
	}
	return s
}

// NewWithStore wires already-constructed dependencies (mainly tests).
func NewWithStore(book *depth.Book, engine *strategy.Engine, tracker *plan.Tracker, store *persist.Store) *Service {
	return &Service{
		book:    book,
		engine:  engine,
		tracker: tracker,
		store:   store,
		now:     time.Now,
	}
}

// HandleSnapshot installs a full depth snapshot and commits the new
// position.
func (s *Service) HandleSnapshot(snap model.Snapshot) error {
	if err := s.book.Reset(snap); err != nil {
		return err
	}
	return s.commit()
}

// HandleUpdate applies an incremental depth update and commits the
// resulting position. Reorders/duplicates are handled inside the book.
func (s *Service) HandleUpdate(u model.DepthUpdate) error {
	if err := s.book.Apply(u); err != nil {
		return err
	}
	return s.commit()
}

// Estimate builds a plan from one consistent depth snapshot.
func (s *Service) Estimate(req model.EstimateRequest) (*model.Report, error) {
	if req.Quantity <= 0 || req.Horizon.Duration() <= 0 || req.Symbol == "" {
		return nil, model.ErrInvalidArgument
	}
	// Take the full consistent view first; only then build the plan.
	view, err := s.book.View(req.Symbol)
	if err != nil {
		return nil, err // ErrEmptyDepth or depth.ErrStaleDepth
	}

	levels := view.Asks
	if req.Side == model.SideSell {
		levels = view.Bids
	}
	covered, _ := strategy.WalkLevels(levels, req.Quantity)
	insufficient := covered+1e-12 < req.Quantity
	if insufficient && req.RejectIfInsufficient {
		return nil, fmt.Errorf("%w: available %.8f requested %.8f",
			model.ErrInsufficientDepth, covered, req.Quantity)
	}

	p, err := s.engine.BuildPlan(req, view, s.now())
	if err != nil {
		return nil, err
	}
	p.ID = s.newID()

	s.mu.Lock()
	s.tracker.Put(p)
	commitErr := s.commitLocked()
	s.mu.Unlock()
	if commitErr != nil {
		return nil, commitErr
	}

	report := buildReport(p, view.Seq)
	if insufficient {
		report.Degraded = &model.Degraded{
			Reason:    "visible depth does not cover the full quantity; estimates assume the same impact model continues past the visible book",
			Available: covered,
			Requested: req.Quantity,
		}
	}
	return report, nil
}

// Settle marks a slice settled, recording it durably.
func (s *Service) Settle(planID string, sliceIndex int) (*model.Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.tracker.Settle(planID, sliceIndex); err != nil {
		return nil, err
	}
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	p, _ := s.tracker.Get(planID)
	return p, nil
}

// Cancel cancels a plan: later slices are excluded from totals and risk.
func (s *Service) Cancel(planID string) (*model.Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.tracker.Cancel(planID); err != nil {
		return nil, err
	}
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	p, _ := s.tracker.Get(planID)
	return p, nil
}

// GetPlan returns a plan copy.
func (s *Service) GetPlan(id string) (*model.Plan, error) {
	p, ok := s.tracker.Get(id)
	if !ok {
		return nil, model.ErrPlanNotFound
	}
	return p, nil
}

// Recover restores plans and primes depth positions from the last
// committed state after restart. Unfinished plans remain distinguishable
// by status; the book stays empty until a fresh snapshot arrives, so
// late or duplicate updates cannot cause drift.
func (s *Service) Recover() error {
	if s.store == nil {
		return nil
	}
	st, err := s.store.Load()
	if err != nil {
		return err
	}
	s.tracker.ReplaceAll(st.Plans)
	for _, pos := range st.DepthPositions {
		s.book.PrimePosition(pos.Symbol, pos.LastSeq)
	}
	return nil
}

// Close flushes durable state.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked()
}

// commit persists the current positions and plans.
func (s *Service) commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked()
}

func (s *Service) commitLocked() error {
	if s.store == nil {
		return nil
	}
	st := &persist.State{
		DepthPositions: map[string]persist.DepthPosition{},
		Plans:          s.tracker.All(),
	}
	// Record the committed position of every symbol the book knows.
	for _, d := range s.book.States() {
		st.DepthPositions[d.Symbol] = persist.DepthPosition{
			Symbol:  d.Symbol,
			LastSeq: d.LastSeq,
			Stale:   d.Stale,
		}
	}
	return s.store.Save(st)
}

func (s *Service) newID() string {
	n := s.idSeq.Add(1)
	return fmt.Sprintf("plan-%d-%d", s.now().UnixNano(), n)
}

func buildReport(p *model.Plan, seq int64) *model.Report {
	return &model.Report{
		PlanID:        p.ID,
		ViewSeq:       seq,
		Slices:        append([]model.Slice(nil), p.Slices...),
		ArrivalMid:    p.ArrivalMid,
		TotalSlippage: p.TotalSlippage(),
		TotalImpact:   p.TotalImpact(),
		TotalCost:     p.TotalCost(),
		ResidualRisk:  p.ResidualRisk(),
	}
}

// ErrStaleDepth is re-exported for callers that want to classify errors.
var ErrStaleDepth = depth.ErrStaleDepth

// IsDepthError reports whether err is a depth availability error.
func IsDepthError(err error) bool {
	return errors.Is(err, model.ErrEmptyDepth) || errors.Is(err, depth.ErrStaleDepth)
}
