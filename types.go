package estimator

import (
	"errors"
	"time"
)

// Side is the order direction.
type Side int

const (
	Buy Side = iota + 1
	Sell
)

func (s Side) String() string {
	switch s {
	case Buy:
		return "BUY"
	case Sell:
		return "SELL"
	default:
		return "UNKNOWN"
	}
}

// PriceLevel is one depth level.
type PriceLevel struct {
	Price float64
	Size  float64
}

// DepthUpdate is a sequenced depth message. When Snapshot is true the message
// is a full snapshot and resets the book; otherwise it is an incremental
// update where each level replaces the size at that price (size 0 removes).
type DepthUpdate struct {
	Symbol     string
	Seq        uint64
	Snapshot   bool
	Bids       []PriceLevel
	Asks       []PriceLevel
	ReceivedAt time.Time
}

// Strategy selects a slicing strategy.
type Strategy string

const (
	// TWAP slices the order into equally sized slices.
	TWAP Strategy = "TWAP"
	// VWAP slices the order with a deterministic U-shaped intraday pattern.
	VWAP Strategy = "VWAP"
	// IS (implementation shortfall) front-loads slices.
	IS Strategy = "IS"
)

// Config holds model parameters.
type Config struct {
	// Symbol is the trading symbol this service instance tracks.
	Symbol string
	// ImpactCoeff is the eta coefficient of the market impact model.
	ImpactCoeff float64
	// ImpactExponent is the participation exponent (0.5 = square root).
	ImpactExponent float64
	// DecayRate is the per-slice decay of persistent impact (0..1).
	DecayRate float64
	// MaxSlices caps the number of slices per plan.
	MaxSlices int
	// MinSliceSeconds is the shortest time span represented by one slice.
	MinSliceSeconds float64
	// Volatility is per-horizon volatility used for residual risk (fraction).
	Volatility float64
	// RiskAversion scales residual risk.
	RiskAversion float64
	// Seed is the default RNG seed; per-request seeds override it.
	Seed int64
}

// EstimateRequest is an estimation request.
type EstimateRequest struct {
	Symbol   string
	Side     Side
	Quantity float64
	Horizon  time.Duration
	Strategy Strategy
	// Seed, when non-zero, overrides Config.Seed for reproducible projections.
	Seed int64
}

// SliceStatus is the lifecycle state of a slice.
type SliceStatus string

const (
	SlicePending  SliceStatus = "PENDING"
	SliceSettled  SliceStatus = "SETTLED"
	SliceCanceled SliceStatus = "CANCELED"
)

// SliceReport is the per-slice cost projection / settlement record.
type SliceReport struct {
	Index        int
	Offset       time.Duration
	Quantity     float64
	Mid          float64
	FillPrice    float64
	SlippageCost float64
	ImpactCost   float64
	TotalCost    float64
	ResidualRisk float64
	RealizedCost float64
	Status       SliceStatus
}

// PlanStatus is the lifecycle state of a plan.
type PlanStatus string

const (
	PlanRunning   PlanStatus = "RUNNING"
	PlanCompleted PlanStatus = "COMPLETED"
	PlanCanceled  PlanStatus = "CANCELED"
)

// PlanReport is the full, traceable view of an execution plan.
type PlanReport struct {
	ID                     string
	Symbol                 string
	Side                   Side
	Strategy               Strategy
	TotalQuantity          float64
	Horizon                time.Duration
	Status                 PlanStatus
	Slices                 []SliceReport
	SettledCost            float64
	ProjectedRemainingCost float64
	TotalCost              float64
	RemainingRisk          float64
	BaseDepthSeq           uint64
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

var (
	// ErrDepthUnavailable means the book is empty or has never reached a
	// consistent state; estimation must be rejected rather than assume zero.
	ErrDepthUnavailable = errors.New("estimator: depth book is unavailable or inconsistent")
	// ErrDepthInsufficient means the visible depth cannot absorb the request.
	ErrDepthInsufficient = errors.New("estimator: order book depth insufficient for requested size")
	// ErrInvalidArgument is returned for malformed requests.
	ErrInvalidArgument = errors.New("estimator: invalid argument")
	// ErrPlanNotFound is returned when a plan id is unknown.
	ErrPlanNotFound = errors.New("estimator: plan not found")
	// ErrPlanClosed is returned when mutating a completed/canceled plan.
	ErrPlanClosed = errors.New("estimator: plan already closed")
	// ErrSliceSettled is returned when a settled slice is changed again.
	ErrSliceSettled = errors.New("estimator: slice already settled")
)
