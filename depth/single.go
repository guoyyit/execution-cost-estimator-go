package depth

import (
	"sort"
	"time"

	"github.com/guoyyit/execution-cost-estimator-go/model"
)

// maxBufferedUpdates bounds the reorder buffer. A real feed gap is
// healed either by the late update arriving or by a fresh snapshot.
const maxBufferedUpdates = 4096

// singleBook is one symbol's book plus the reorder buffer.
// All access happens under Book.mu.
type singleBook struct {
	symbol  string
	lastSeq int64
	ts      time.Time
	hasSnap bool
	// stale is true exactly when an update with seq > lastSeq+1 has been
	// observed while lastSeq+1 has not been applied. The committed book
	// state always stays the prefix [snapshot..lastSeq], so a stale book
	// is still internally consistent at lastSeq - just behind.
	stale  bool
	bids   map[float64]float64
	asks   map[float64]float64
	buffer map[int64]model.DepthUpdate
}

func newSingleBook(symbol string) *singleBook {
	return &singleBook{
		symbol: symbol,
		bids:   make(map[float64]float64),
		asks:   make(map[float64]float64),
		buffer: make(map[int64]model.DepthUpdate),
	}
}

// reset replaces the book with an authoritative snapshot. Any buffered
// updates newer than the snapshot are replayed contiguously.
func (sb *singleBook) reset(v model.DepthView) {
	sb.hasSnap = true
	sb.lastSeq = v.Seq
	if !v.Timestamp.IsZero() {
		sb.ts = v.Timestamp
	}
	sb.bids = make(map[float64]float64, len(v.Bids))
	sb.asks = make(map[float64]float64, len(v.Asks))
	for _, l := range v.Bids {
		if l.Size > 0 {
			sb.bids[l.Price] = l.Size
		}
	}
	for _, l := range v.Asks {
		if l.Size > 0 {
			sb.asks[l.Price] = l.Size
		}
	}
	for seq := range sb.buffer {
		if seq <= v.Seq {
			delete(sb.buffer, seq)
		}
	}
	sb.drainBufferLocked()
}

// ingest is the update state machine.
func (sb *singleBook) ingest(u model.DepthUpdate) {
	if !sb.hasSnap {
		// Before the first post-restart snapshot only the committed
		// position is known: discard replayed duplicates/late updates
		// and buffer genuine future ones without fabricating levels.
		if u.Seq <= sb.lastSeq {
			return
		}
		if _, ok := sb.buffer[u.Seq]; !ok {
			sb.bufferUpdate(u)
			sb.stale = len(sb.buffer) > 0
		}
		return
	}
	if u.Seq <= sb.lastSeq {
		// Duplicate / already covered: a full rebuild would ignore it too.
		return
	}
	if u.Seq == sb.lastSeq+1 {
		sb.applyUpdate(u)
		sb.drainBufferLocked()
		return
	}
	// u.Seq > lastSeq+1: a gap appeared. Hold the update; the book is
	// stale until the missing update arrives (possibly late) or a reset.
	sb.bufferUpdate(u)
	sb.stale = len(sb.buffer) > 0
}

// bufferUpdate stores one future update, ignoring duplicates and
// dropping new entries once the cap is reached.
func (sb *singleBook) bufferUpdate(u model.DepthUpdate) {
	if _, ok := sb.buffer[u.Seq]; ok {
		return
	}
	if len(sb.buffer) >= maxBufferedUpdates {
		return
	}
	sb.buffer[u.Seq] = u
}

// drainBufferLocked applies every contiguous update starting at
// lastSeq+1, then re-evaluates staleness.
func (sb *singleBook) drainBufferLocked() {
	for {
		u, ok := sb.buffer[sb.lastSeq+1]
		if !ok {
			break
		}
		delete(sb.buffer, sb.lastSeq+1)
		sb.applyUpdate(u)
	}
	// Any remaining buffered update implies a gap ahead.
	sb.stale = len(sb.buffer) > 0
}

// applyUpdate merges one update known to be the next sequence.
func (sb *singleBook) applyUpdate(u model.DepthUpdate) {
	for _, l := range u.Bids {
		if l.Size <= 0 {
			delete(sb.bids, l.Price)
		} else {
			sb.bids[l.Price] = l.Size
		}
	}
	for _, l := range u.Asks {
		if l.Size <= 0 {
			delete(sb.asks, l.Price)
		} else {
			sb.asks[l.Price] = l.Size
		}
	}
	sb.lastSeq = u.Seq
	if !u.Timestamp.IsZero() {
		sb.ts = u.Timestamp
	}
}

// snapshot builds an immutable, ordered, deep-copied view.
func (sb *singleBook) snapshot() model.DepthView {
	bids := make([]model.PriceLevel, 0, len(sb.bids))
	for price, size := range sb.bids {
		if size > 0 {
			bids = append(bids, model.PriceLevel{Price: price, Size: size})
		}
	}
	asks := make([]model.PriceLevel, 0, len(sb.asks))
	for price, size := range sb.asks {
		if size > 0 {
			asks = append(asks, model.PriceLevel{Price: price, Size: size})
		}
	}
	sort.Slice(bids, func(i, j int) bool { return bids[i].Price > bids[j].Price })
	sort.Slice(asks, func(i, j int) bool { return asks[i].Price < asks[j].Price })
	return model.DepthView{
		Symbol:    sb.symbol,
		Seq:       sb.lastSeq,
		Timestamp: sb.ts,
		Bids:      bids,
		Asks:      asks,
	}
}
