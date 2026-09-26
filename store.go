package estimator

import "time"

// Store is the committed-state persistence boundary. Save must be atomic and
// durably committed; Load returns the most recently committed state.
type Store interface {
	Load() (PersistedState, error)
	Save(state PersistedState) error
}

// persistedDepth is the serializable book state.
type persistedDepth struct {
	Symbol    string       `json:"symbol"`
	Bids      []PriceLevel `json:"bids"`
	Asks      []PriceLevel `json:"asks"`
	LastSeq   uint64       `json:"last_seq"`
	Valid     bool         `json:"valid"`
	HasGap    bool         `json:"has_gap"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// persistedPlan is the serializable plan state.
type persistedPlan struct {
	ID        string        `json:"id"`
	Symbol    string        `json:"symbol"`
	Side      Side          `json:"side"`
	Strategy  Strategy      `json:"strategy"`
	TotalQty  float64       `json:"total_qty"`
	Horizon   time.Duration `json:"horizon"`
	Status    PlanStatus    `json:"status"`
	Slices    []SliceReport `json:"slices"`
	BaseSeq   uint64        `json:"base_seq"`
	Seed      int64         `json:"seed"`
	Weights   []float64     `json:"weights"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// PersistedState is everything committed in one transactional write.
type PersistedState struct {
	Depth persistedDepth  `json:"depth"`
	Plans []persistedPlan `json:"plans"`
}

func (b *depthBook) toPersisted() persistedDepth {
	return persistedDepth{
		Symbol:    b.symbol,
		Bids:      sortedLevels(b.bids, false),
		Asks:      sortedLevels(b.asks, true),
		LastSeq:   b.lastSeq,
		Valid:     b.valid,
		HasGap:    b.hasGap(),
		UpdatedAt: b.updatedAt,
	}
}

func depthFromPersisted(pd persistedDepth) *depthBook {
	b := newDepthBook(pd.Symbol)
	b.lastSeq = pd.LastSeq
	b.valid = pd.Valid
	b.updatedAt = pd.UpdatedAt
	mergeSide(b.bids, pd.Bids)
	mergeSide(b.asks, pd.Asks)
	return b
}
