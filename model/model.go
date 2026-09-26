// Package model defines the shared domain types for the execution cost
// estimation service: order book depth, execution plans, slices and reports.
package model

import (
	"errors"
	"strconv"
	"time"
)

// Side of an order.
type Side int

const (
	SideBuy Side = iota + 1
	SideSell
)

// Strategy selects the slicing / impact model.
type Strategy int

const (
	StrategyTWAP Strategy = iota + 1
	StrategyVWAP
	StrategyPOV
)

// SliceStatus is the lifecycle state of a single execution slice.
type SliceStatus int

const (
	SlicePending SliceStatus = iota
	SliceSettled
	SliceCancelled
)

// PlanStatus is the lifecycle state of an execution plan.
type PlanStatus int

const (
	PlanActive PlanStatus = iota
	PlanCancelled
	PlanCompleted
)

// PriceLevel is one side of one depth level.
type PriceLevel struct {
	Price float64 `json:"price"`
	Size  float64 `json:"size"`
}

// DepthView is an immutable point-in-time view of an order book.
type DepthView struct {
	Symbol    string       `json:"symbol"`
	Seq       int64        `json:"seq"`
	Timestamp time.Time    `json:"timestamp"`
	Bids      []PriceLevel `json:"bids"` // descending price
	Asks      []PriceLevel `json:"asks"` // ascending price
}

// MidPrice returns the mid of best bid / ask. It is 0 on a one-sided
// book; callers must treat 0 as "not available" rather than "free".
func (v *DepthView) MidPrice() float64 {
	if len(v.Bids) == 0 || len(v.Asks) == 0 {
		return 0
	}
	return (v.Bids[0].Price + v.Asks[0].Price) / 2
}

// Spread returns best ask - best bid.
func (v *DepthView) Spread() float64 {
	if len(v.Bids) == 0 || len(v.Asks) == 0 {
		return 0
	}
	return v.Asks[0].Price - v.Bids[0].Price
}

// BestPrice returns the ask for buys and the bid for sells.
func (v *DepthView) BestPrice(s Side) float64 {
	switch s {
	case SideBuy:
		if len(v.Asks) > 0 {
			return v.Asks[0].Price
		}
	case SideSell:
		if len(v.Bids) > 0 {
			return v.Bids[0].Price
		}
	}
	return 0
}

// DepthUpdate is an incremental depth change.
type DepthUpdate struct {
	Symbol    string       `json:"symbol"`
	Seq       int64        `json:"seq"`
	Timestamp time.Time    `json:"timestamp"`
	Bids      []PriceLevel `json:"bids"` // a zero size removes the price level
	Asks      []PriceLevel `json:"asks"`
}

// Snapshot is a full depth reset with an authoritative sequence number.
type Snapshot struct {
	View DepthView
}

// DurationMS is a time.Duration that serialises to/from whole
// milliseconds in JSON.
type DurationMS time.Duration

// MarshalJSON emits the duration in milliseconds.
func (d DurationMS) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(time.Duration(d).Milliseconds(), 10)), nil
}

// UnmarshalJSON accepts milliseconds.
func (d *DurationMS) UnmarshalJSON(b []byte) error {
	ms, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return err
	}
	*d = DurationMS(time.Duration(ms) * time.Millisecond)
	return nil
}

// Duration returns the underlying time.Duration.
func (d DurationMS) Duration() time.Duration { return time.Duration(d) }

// Params configure slicing, impact modelling and the deterministic RNG.
type Params struct {
	ImpactCoeff float64 `json:"impact_coeff"` // eta in the impact model
	DecayRate   float64 `json:"decay_rate"`   // impact decay between slices
	MaxSlices   int     `json:"max_slices"`   // hard cap on slice count
	RiskAnnual  float64 `json:"risk_annual"`  // annualised volatility for residual risk
	Seed        int64   `json:"seed"`         // deterministic RNG seed
	Participate float64 `json:"participate"`  // participation rate for POV (0..1]
	RNG         RNGKind `json:"rng"`          // noise model: 0 none, 1 uniform
}

// RNGKind selects the deterministic noise source.
type RNGKind int

const (
	RNGNone RNGKind = iota
	RNGUniform
)

// Slice is one scheduled execution tranche.
type Slice struct {
	Index        int         `json:"index"`
	ScheduledAt  time.Time   `json:"scheduled_at"`
	Quantity     float64     `json:"quantity"`
	Price        float64     `json:"price"`         // expected execution price
	Slippage     float64     `json:"slippage"`      // vs arrival mid, per unit
	SpreadCost   float64     `json:"spread_cost"`   // half-spread paid, per unit
	Impact       float64     `json:"impact"`        // temporary market impact, per unit
	ResidualRisk float64     `json:"residual_risk"` // risk carried after this slice, currency
	Status       SliceStatus `json:"status"`        // 0 pending, 1 settled, 2 cancelled
	SettledAt    time.Time   `json:"settled_at"`
}

// Plan is a scheduled execution and its settled progress.
type Plan struct {
	ID          string     `json:"id"`
	Symbol      string     `json:"symbol"`
	Side        Side       `json:"side"`
	Quantity    float64    `json:"quantity"`
	Horizon     DurationMS `json:"horizon_ms"`
	Strategy    Strategy   `json:"strategy"`
	Params      Params     `json:"params"`
	CreatedAt   time.Time  `json:"created_at"`
	Status      PlanStatus `json:"status"` // 0 active, 1 cancelled, 2 completed
	Slices      []Slice    `json:"slices"`
	ArrivalMid  float64    `json:"arrival_mid"`
	CancelledAt time.Time  `json:"cancelled_at"`
}

// SlippageCost returns the positive cost per unit of executing at
// Price relative to arrival mid for the given side.
func SlippageCost(side Side, arrivalMid, price float64) float64 {
	if side == SideBuy {
		return price - arrivalMid
	}
	return arrivalMid - price
}

// TotalCost is the currency cost of all non-cancelled slices
// (quantity * per-unit slippage).
func (p *Plan) TotalCost() float64 {
	var total float64
	for i := range p.Slices {
		sl := &p.Slices[i]
		if sl.Status == SliceCancelled {
			continue
		}
		total += sl.Quantity * sl.Slippage
	}
	return total
}

// TotalSlippage is the quantity-weighted average per-unit slippage
// across non-cancelled slices.
func (p *Plan) TotalSlippage() float64 {
	var qty, cost float64
	for i := range p.Slices {
		sl := &p.Slices[i]
		if sl.Status == SliceCancelled {
			continue
		}
		qty += sl.Quantity
		cost += sl.Quantity * sl.Slippage
	}
	if qty == 0 {
		return 0
	}
	return cost / qty
}

// TotalImpact is the currency value of temporary market impact across
// non-cancelled slices.
func (p *Plan) TotalImpact() float64 {
	var total float64
	for i := range p.Slices {
		sl := &p.Slices[i]
		if sl.Status == SliceCancelled {
			continue
		}
		total += sl.Quantity * sl.Impact
	}
	return total
}

// FilledQuantity returns the quantity of settled slices.
func (p *Plan) FilledQuantity() float64 {
	var qty float64
	for i := range p.Slices {
		if p.Slices[i].Status == SliceSettled {
			qty += p.Slices[i].Quantity
		}
	}
	return qty
}

// ResidualRisk returns the risk carried by the remaining non-cancelled
// slices: the residual-risk value of the last such slice (0 if the
// plan was cancelled before any slice or all slices settled to zero).
func (p *Plan) ResidualRisk() float64 {
	risk := 0.0
	for i := range p.Slices {
		sl := &p.Slices[i]
		if sl.Status == SliceCancelled {
			continue
		}
		risk = sl.ResidualRisk
	}
	return risk
}

// EstimateRequest starts a new estimation / plan.
type EstimateRequest struct {
	Symbol   string     `json:"symbol"`
	Side     Side       `json:"side"`
	Quantity float64    `json:"quantity"`
	Horizon  DurationMS `json:"horizon_ms"`
	Strategy Strategy   `json:"strategy"`
	Params   Params     `json:"params"`
	// RejectIfInsufficient makes Estimate return ErrInsufficientDepth
	// when visible depth cannot cover Quantity. Otherwise Estimate
	// succeeds but the Report carries a non-nil Degraded notice.
	// Empty / stale depth is always rejected, never treated as zero.
	RejectIfInsufficient bool `json:"reject_if_insufficient"`
}

// Degraded describes an estimation that ran with insufficient depth.
type Degraded struct {
	Reason    string  `json:"reason"`
	Available float64 `json:"available"` // quantity coverable by available depth
	Requested float64 `json:"requested"`
}

// Report is the result of one estimation.
type Report struct {
	PlanID        string    `json:"plan_id"`
	ViewSeq       int64     `json:"view_seq"`
	Slices        []Slice   `json:"slices"`
	ArrivalMid    float64   `json:"arrival_mid"`
	TotalSlippage float64   `json:"total_slippage"`
	TotalImpact   float64   `json:"total_impact"`
	TotalCost     float64   `json:"total_cost"`
	ResidualRisk  float64   `json:"residual_risk"`
	Degraded      *Degraded `json:"degraded,omitempty"`
}

// Sentinel errors.
var (
	ErrEmptyDepth        = errors.New("depth is empty or unavailable")
	ErrInsufficientDepth = errors.New("insufficient depth for the requested quantity")
	ErrPlanNotFound      = errors.New("plan not found")
	ErrPlanCancelled     = errors.New("plan is cancelled")
	ErrInvalidArgument   = errors.New("invalid argument")
)
