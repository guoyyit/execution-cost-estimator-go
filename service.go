package estimator

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Service is the local execution cost estimation service.
//
// Concurrency model: rwMu is a read/write lock over the depth book. Every
// estimate holds rwMu for its whole read and builds from a single copied
// DepthSnapshot, while depth updates take the write lock; an estimate can
// therefore never observe a field-level mixed intermediate book. Plan
// lifecycle state lives behind plansMu. The global lock order is always
// rwMu -> plansMu. Persistence is performed after both locks are released.
type Service struct {
	cfg   Config
	book  *depthBook
	model *impactModel
	store Store
	now   func() time.Time
	idSeq uint64

	rwMu    sync.RWMutex
	plansMu sync.Mutex
	plans   map[string]*plan
}

// NewService constructs a service and, when store != nil, restores from the
// last committed position. The restored depth keeps its committed seq so
// duplicate/late increments are ignored; plans resume with their exact
// settled/pending/canceled slice states.
func NewService(cfg Config, store Store) (*Service, error) {
	cfg = withDefaults(cfg)
	if cfg.Symbol == "" {
		return nil, fmt.Errorf("%w: empty symbol", ErrInvalidArgument)
	}
	s := &Service{
		cfg:   cfg,
		book:  newDepthBook(cfg.Symbol),
		model: newImpactModel(cfg),
		store: store,
		now:   time.Now,
		plans: map[string]*plan{},
	}
	if store != nil {
		state, err := store.Load()
		if err != nil {
			return nil, fmt.Errorf("estimator: restore: %w", err)
		}
		s.restore(state)
	}
	return s, nil
}

func withDefaults(c Config) Config {
	if c.ImpactCoeff == 0 {
		c.ImpactCoeff = 0.1
	}
	if c.ImpactExponent == 0 {
		c.ImpactExponent = 0.5
	}
	if c.MaxSlices <= 0 {
		c.MaxSlices = 1000
	}
	if c.MinSliceSeconds <= 0 {
		c.MinSliceSeconds = 60
	}
	if c.Volatility == 0 {
		c.Volatility = 0.01
	}
	if c.RiskAversion == 0 {
		c.RiskAversion = 1
	}
	if c.Seed == 0 {
		c.Seed = 1
	}
	return c
}

func (s *Service) restore(state PersistedState) {
	if state.Depth.Symbol == s.cfg.Symbol {
		s.book = depthFromPersisted(state.Depth)
	}
	for i := range state.Plans {
		p := planFromPersisted(state.Plans[i])
		s.plans[p.id] = p
		s.idSeq++
	}
}

// ApplyDepth handles a snapshot or incremental depth update.
func (s *Service) ApplyDepth(ctx context.Context, u DepthUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if u.Symbol != s.cfg.Symbol {
		return fmt.Errorf("%w: symbol %s", ErrInvalidArgument, u.Symbol)
	}
	if u.ReceivedAt.IsZero() {
		u.ReceivedAt = s.now()
	}
	s.rwMu.Lock()
	applied := s.book.apply(u)
	s.rwMu.Unlock()
	if !applied {
		return nil // duplicate or buffered out-of-order message
	}
	return s.persist()
}

// DepthSnapshot returns the current consistent snapshot.
func (s *Service) DepthSnapshot(ctx context.Context) (DepthSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return DepthSnapshot{}, err
	}
	s.rwMu.RLock()
	defer s.rwMu.RUnlock()
	snap, ok := s.book.snapshot(s.now())
	if !ok {
		return DepthSnapshot{}, ErrDepthUnavailable
	}
	return snap, nil
}

// Estimate produces a one-off, reproducible cost estimate without persisting
// a plan. It explicitly rejects empty/unavailable or insufficient depth.
func (s *Service) Estimate(ctx context.Context, req EstimateRequest) (PlanReport, error) {
	if err := ctx.Err(); err != nil {
		return PlanReport{}, err
	}
	if err := validateRequest(s.cfg.Symbol, req); err != nil {
		return PlanReport{}, err
	}
	s.rwMu.RLock()
	_, _, report, err := s.projectLocked(req, "")
	s.rwMu.RUnlock()
	return report, err
}

// CreatePlan persists an executable plan and returns its traceable report.
func (s *Service) CreatePlan(ctx context.Context, req EstimateRequest) (PlanReport, error) {
	if err := ctx.Err(); err != nil {
		return PlanReport{}, err
	}
	if err := validateRequest(s.cfg.Symbol, req); err != nil {
		return PlanReport{}, err
	}
	now := s.now()

	s.rwMu.RLock()
	seed, weights, report, err := s.projectLocked(req, "")
	s.rwMu.RUnlock()
	if err != nil {
		return PlanReport{}, err
	}

	s.plansMu.Lock()
	s.idSeq++
	id := fmt.Sprintf("plan-%d-%d", now.UnixNano(), s.idSeq)
	p := newPlan(id, req, report.BaseDepthSeq, seed, weights, report.Slices, now)
	s.plans[id] = p
	out := p.report()
	s.plansMu.Unlock()

	if err := s.persist(); err != nil {
		return PlanReport{}, err
	}
	return out, nil
}

// projectLocked must be called with rwMu held (read or write). It returns the
// seed and weights used alongside the assembled report.
func (s *Service) projectLocked(req EstimateRequest, id string) (int64, []float64, PlanReport, error) {
	snap, ok := s.book.snapshot(s.now())
	if !ok {
		// Explicit degradation: never treat missing depth as zero depth.
		return 0, nil, PlanReport{}, ErrDepthUnavailable
	}
	n := s.model.numSlices(req.Horizon.Seconds())
	weights := s.model.sliceWeights(req.Strategy, n)
	seed := req.Seed
	if seed == 0 {
		seed = s.cfg.Seed
	}
	rng := s.model.rng(seed)
	slices, err := s.model.project(req, snap, weights, rng)
	if err != nil {
		return 0, nil, PlanReport{}, err
	}
	report := PlanReport{
		ID:            id,
		Symbol:        req.Symbol,
		Side:          req.Side,
		Strategy:      req.Strategy,
		TotalQuantity: req.Quantity,
		Horizon:       req.Horizon,
		Status:        PlanRunning,
		Slices:        slices,
		BaseDepthSeq:  snap.Seq,
		CreatedAt:     s.now(),
		UpdatedAt:     s.now(),
	}
	for _, sl := range slices {
		report.ProjectedRemainingCost += sl.TotalCost
		report.TotalCost += sl.TotalCost
		report.RemainingRisk += sl.ResidualRisk
	}
	return seed, weights, report, nil
}

// GetPlan returns the current report of a plan.
func (s *Service) GetPlan(ctx context.Context, id string) (PlanReport, error) {
	if err := ctx.Err(); err != nil {
		return PlanReport{}, err
	}
	s.plansMu.Lock()
	p, ok := s.plans[id]
	var out PlanReport
	if ok {
		out = p.report()
	}
	s.plansMu.Unlock()
	if !ok {
		return PlanReport{}, ErrPlanNotFound
	}
	return out, nil
}

// Settle records a realized slice cost.
func (s *Service) Settle(ctx context.Context, id string, idx int, realizedCost float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if realizedCost < 0 {
		return fmt.Errorf("%w: negative realized cost", ErrInvalidArgument)
	}
	s.plansMu.Lock()
	p, ok := s.plans[id]
	if !ok {
		s.plansMu.Unlock()
		return ErrPlanNotFound
	}
	err := p.settle(idx, realizedCost, s.now())
	s.plansMu.Unlock()
	if err != nil {
		return err
	}
	return s.persist()
}

// UpdatePlan changes the remaining quantity/horizon of a running plan. The
// remainder is re-projected from the current consistent snapshot.
func (s *Service) UpdatePlan(ctx context.Context, id string, totalQty float64, horizon time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if totalQty <= 0 || horizon <= 0 {
		return fmt.Errorf("%w: quantity and horizon must be positive", ErrInvalidArgument)
	}
	s.rwMu.RLock()
	snap, ok := s.book.snapshot(s.now())
	s.rwMu.RUnlock()
	if !ok {
		return ErrDepthUnavailable
	}

	s.plansMu.Lock()
	p, exists := s.plans[id]
	if !exists {
		s.plansMu.Unlock()
		return ErrPlanNotFound
	}
	reproject := func(side Side, remainingQty float64, h time.Duration) ([]SliceReport, error) {
		if remainingQty == 0 {
			return nil, nil
		}
		req := EstimateRequest{
			Symbol:   s.cfg.Symbol,
			Side:     side,
			Quantity: remainingQty,
			Horizon:  h,
			Strategy: p.strategy,
			Seed:     p.seed,
		}
		n := s.model.numSlices(h.Seconds())
		weights := s.model.sliceWeights(req.Strategy, n)
		return s.model.project(req, snap, weights, s.model.rng(p.seed))
	}
	err := p.update(totalQty, horizon, reproject, s.now())
	s.plansMu.Unlock()
	if err != nil {
		return err
	}
	return s.persist()
}

// CancelPlan cancels a running plan.
func (s *Service) CancelPlan(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.plansMu.Lock()
	p, ok := s.plans[id]
	if !ok {
		s.plansMu.Unlock()
		return ErrPlanNotFound
	}
	err := p.cancel(s.now())
	s.plansMu.Unlock()
	if err != nil {
		return err
	}
	return s.persist()
}

// persist captures a fully consistent state and commits it. No service lock
// is held while writing.
func (s *Service) persist() error {
	if s.store == nil {
		return nil
	}
	state := s.captureState()
	return s.store.Save(state)
}

func (s *Service) captureState() PersistedState {
	s.rwMu.RLock()
	depth := s.book.toPersisted()
	s.rwMu.RUnlock()

	s.plansMu.Lock()
	plans := make([]persistedPlan, 0, len(s.plans))
	for _, p := range s.plans {
		plans = append(plans, p.toPersisted())
	}
	s.plansMu.Unlock()

	return PersistedState{Depth: depth, Plans: plans}
}

func validateRequest(symbol string, req EstimateRequest) error {
	if req.Symbol != symbol {
		return fmt.Errorf("%w: symbol %s", ErrInvalidArgument, req.Symbol)
	}
	if req.Side != Buy && req.Side != Sell {
		return fmt.Errorf("%w: side", ErrInvalidArgument)
	}
	if req.Quantity <= 0 {
		return fmt.Errorf("%w: quantity must be positive", ErrInvalidArgument)
	}
	if req.Horizon <= 0 {
		return fmt.Errorf("%w: horizon must be positive", ErrInvalidArgument)
	}
	if req.Strategy != "" && req.Strategy != TWAP && req.Strategy != VWAP && req.Strategy != IS {
		return fmt.Errorf("%w: strategy %s", ErrInvalidArgument, req.Strategy)
	}
	return nil
}
