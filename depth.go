package estimator

import (
	"sort"
	"time"
)

// bookSide stores one side of the book keyed by price.
type bookSide map[float64]float64

// depthBook is an order book reconstructed from sequenced snapshots/updates.
// It is only ever accessed while the service holds its read/write lock; the
// book itself never exposes its internal maps.
//
// Sequencing rules (the result never diverges from a full rebuild):
//   - A snapshot at seq S resets the book exactly to that image and clears
//     any buffered increments, regardless of prior seq.
//   - Incremental updates apply only as a contiguous run lastSeq+1, lastSeq+2,
//     ... . Messages that arrive early are buffered by seq; when the missing
//     link arrives the whole run is applied in order.
//   - An increment at seq <= lastSeq is a duplicate/late message and ignored.
//   - A permanently missing increment simply leaves the book at its last
//     complete (older) image until the next snapshot re-anchors it.
type depthBook struct {
	symbol    string
	bids      bookSide
	asks      bookSide
	lastSeq   uint64
	valid     bool
	pending   map[uint64]DepthUpdate
	updatedAt time.Time
}

func newDepthBook(symbol string) *depthBook {
	return &depthBook{
		symbol:  symbol,
		bids:    bookSide{},
		asks:    bookSide{},
		pending: map[uint64]DepthUpdate{},
	}
}

// apply ingests a sequenced update and reports whether it changed the book.
func (b *depthBook) apply(u DepthUpdate) bool {
	if u.Symbol != b.symbol {
		return false
	}
	if u.Snapshot {
		b.resetFrom(u)
		return true
	}
	// Duplicate / late increment.
	if u.Seq <= b.lastSeq {
		return false
	}
	// Contiguous: apply now and drain any buffered successors.
	if u.Seq == b.lastSeq+1 {
		b.applyIncrement(u)
		b.drainPending()
		return true
	}
	// Out of order: buffer (later copy of the same seq is identical).
	b.pending[u.Seq] = u
	return false
}

// hasGap reports whether buffered updates wait on a missing predecessor.
func (b *depthBook) hasGap() bool {
	if len(b.pending) == 0 {
		return false
	}
	// If the immediate successor is buffered, the run can drain; otherwise
	// the chain is broken at lastSeq+1.
	_, hasNext := b.pending[b.lastSeq+1]
	return !hasNext
}

func (b *depthBook) drainPending() {
	for {
		u, ok := b.pending[b.lastSeq+1]
		if !ok {
			return
		}
		delete(b.pending, b.lastSeq+1)
		b.applyIncrement(u)
	}
}

func (b *depthBook) applyIncrement(u DepthUpdate) {
	mergeSide(b.bids, u.Bids)
	mergeSide(b.asks, u.Asks)
	b.lastSeq = u.Seq
	if !u.ReceivedAt.IsZero() {
		b.updatedAt = u.ReceivedAt
	}
}

func (b *depthBook) resetFrom(u DepthUpdate) {
	bids := make(bookSide, len(u.Bids))
	asks := make(bookSide, len(u.Asks))
	mergeSide(bids, u.Bids)
	mergeSide(asks, u.Asks)
	b.bids = bids
	b.asks = asks
	b.lastSeq = u.Seq
	b.valid = true
	b.pending = map[uint64]DepthUpdate{}
	if !u.ReceivedAt.IsZero() {
		b.updatedAt = u.ReceivedAt
	}
}

// mergeSide merges levels into the side map; size 0 removes the price.
func mergeSide(dst bookSide, levels []PriceLevel) {
	for _, l := range levels {
		if l.Size <= 0 {
			delete(dst, l.Price)
			continue
		}
		dst[l.Price] = l.Size
	}
}

// complete reports whether the book reflects a tradeable image: anchored by a
// snapshot and non-empty on both sides. An empty/one-sided image is never
// treated as zero depth by estimators.
func (b *depthBook) complete() bool {
	return b.valid && len(b.bids) > 0 && len(b.asks) > 0
}

// snapshot returns a point-in-time, fully copied view of the book.
func (b *depthBook) snapshot(now time.Time) (DepthSnapshot, bool) {
	if !b.complete() {
		return DepthSnapshot{Symbol: b.symbol, Seq: b.lastSeq}, false
	}
	bids := sortedLevels(b.bids, false)
	asks := sortedLevels(b.asks, true)
	bid := bids[0].Price
	ask := asks[0].Price
	ts := b.updatedAt
	if ts.IsZero() {
		ts = now
	}
	return DepthSnapshot{
		Symbol:    b.symbol,
		Bids:      bids,
		Asks:      asks,
		Seq:       b.lastSeq,
		Mid:       (bid + ask) / 2,
		UpdatedAt: ts,
	}, true
}

// sortedLevels returns levels as a copied, price-sorted slice. ascending=true
// sorts low->high (used for asks); false sorts high->low (used for bids).
func sortedLevels(side bookSide, ascending bool) []PriceLevel {
	out := make([]PriceLevel, 0, len(side))
	for px, sz := range side {
		out = append(out, PriceLevel{Price: px, Size: sz})
	}
	sort.Slice(out, func(i, j int) bool {
		if ascending {
			return out[i].Price < out[j].Price
		}
		return out[i].Price > out[j].Price
	})
	return out
}

// DepthSnapshot is an immutable, fully copied view of the book.
type DepthSnapshot struct {
	Symbol    string
	Bids      []PriceLevel
	Asks      []PriceLevel
	Seq       uint64
	Mid       float64
	UpdatedAt time.Time
}

// levels returns the side hit by an aggressive order: buys lift asks, sells
// hit bids.
func (s DepthSnapshot) levels(side Side) []PriceLevel {
	if side == Buy {
		return s.Asks
	}
	return s.Bids
}

// top returns the best price on the given side.
func top(side Side, s DepthSnapshot) (px float64, ok bool) {
	lv := s.levels(side)
	if len(lv) == 0 {
		return 0, false
	}
	return lv[0].Price, true
}

// FillTier is one price tier consumed while walking the book.
type FillTier struct {
	Price float64
	Size  float64
}

// walk consumes depth starting at the best price on the requested side until
// need is filled. The final tier is clamped to the unfilled remainder. It
// returns false if visible depth is insufficient.
func walk(side Side, s DepthSnapshot, need float64) ([]FillTier, bool) {
	if need <= 0 {
		return nil, true
	}
	var tiers []FillTier
	remaining := need
	for _, l := range s.levels(side) {
		if remaining <= 0 {
			break
		}
		take := l.Size
		if take > remaining {
			take = remaining
		}
		tiers = append(tiers, FillTier{Price: l.Price, Size: take})
		remaining -= take
	}
	return tiers, remaining <= 0
}
