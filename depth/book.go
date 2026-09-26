// Package depth maintains order books from snapshots and ordered
// incremental updates, tolerating duplicate, reordered and missing
// arrivals without ever diverging from a full rebuild.
//
// Semantics:
//   - A Snapshot (with its authoritative seq) resets a symbol's book.
//   - Updates are applied strictly at seq = lastSeq+1. Earlier seqs are
//     duplicates and are ignored. Later seqs are buffered and applied as
//     soon as the gap fills, in whatever order they physically arrived.
//   - While a gap is open the committed book remains the exact prefix up
//     to lastSeq (never a field-mixed intermediate state), and View marks
//     it Stale so callers can reject estimation instead of drifting.
//   - A fresh snapshot heals every gap and discards older buffered data.
package depth

import (
	"errors"
	"sync"

	"github.com/guoyyit/execution-cost-estimator-go/model"
)

// ErrStaleDepth is returned by View when the symbol has an open
// sequence gap: the view would be behind the feed.
var ErrStaleDepth = errors.New("depth is stale due to a sequence gap")

// Book is a thread-safe repository of per-symbol order books.
type Book struct {
	mu    sync.RWMutex
	books map[string]*singleBook
}

// NewBook creates an empty book repository.
func NewBook() *Book {
	return &Book{books: make(map[string]*singleBook)}
}

// Reset replaces a symbol's book with a full snapshot. Buffered updates
// newer than the snapshot are replayed contiguously.
func (b *Book) Reset(snap model.Snapshot) error {
	if snap.View.Symbol == "" {
		return model.ErrInvalidArgument
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sb, ok := b.books[snap.View.Symbol]
	if !ok {
		sb = newSingleBook(snap.View.Symbol)
		b.books[snap.View.Symbol] = sb
	}
	sb.reset(snap.View)
	return nil
}

// Apply applies one incremental update. Out-of-order updates are
// buffered and applied in sequence order; duplicates are ignored; gaps
// mark the book stale until healed by a late update or a snapshot.
func (b *Book) Apply(u model.DepthUpdate) error {
	if u.Symbol == "" || u.Seq <= 0 {
		return model.ErrInvalidArgument
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sb, ok := b.books[u.Symbol]
	if !ok {
		sb = newSingleBook(u.Symbol)
		b.books[u.Symbol] = sb
	}
	sb.ingest(u)
	return nil
}

// View returns a complete, immutable deep copy of a symbol's book at
// its last committed sequence. It returns:
//   - model.ErrEmptyDepth when no snapshot has ever been installed or
//     the book contains no usable levels;
//   - ErrStaleDepth when an unfilled sequence gap makes it behind.
func (b *Book) View(symbol string) (model.DepthView, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	sb, ok := b.books[symbol]
	if !ok || !sb.hasSnap {
		return model.DepthView{}, model.ErrEmptyDepth
	}
	if sb.stale {
		return model.DepthView{}, ErrStaleDepth
	}
	v := sb.snapshot()
	if len(v.Bids) == 0 && len(v.Asks) == 0 {
		return model.DepthView{}, model.ErrEmptyDepth
	}
	return v, nil
}

// PrimePosition records the last committed sequence for a symbol
// without fabricating any levels. Used at startup recovery: until a
// fresh snapshot arrives, duplicate/late updates at or below this seq
// are ignored and the symbol reports empty depth rather than stale,
// zero or drifted estimates.
func (b *Book) PrimePosition(symbol string, lastSeq int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sb, ok := b.books[symbol]
	if !ok {
		sb = newSingleBook(symbol)
		b.books[symbol] = sb
	}
	if !sb.hasSnap && lastSeq > sb.lastSeq {
		sb.lastSeq = lastSeq
	}
}

// State reports synchronisation diagnostics for a symbol.
type State struct {
	Symbol      string
	LastSeq     int64
	Stale       bool
	BufferSize  int
	HasSnapshot bool
	Levels      int
}

// States returns diagnostics for every known symbol.
func (b *Book) States() []State {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]State, 0, len(b.books))
	for symbol, sb := range b.books {
		out = append(out, State{
			Symbol:      symbol,
			LastSeq:     sb.lastSeq,
			Stale:       sb.stale,
			BufferSize:  len(sb.buffer),
			HasSnapshot: sb.hasSnap,
			Levels:      len(sb.bids) + len(sb.asks),
		})
	}
	return out
}

// StateOf returns diagnostics.
func (b *Book) StateOf(symbol string) State {
	b.mu.RLock()
	defer b.mu.RUnlock()
	sb, ok := b.books[symbol]
	if !ok {
		return State{Symbol: symbol}
	}
	return State{
		Symbol:      symbol,
		LastSeq:     sb.lastSeq,
		Stale:       sb.stale,
		BufferSize:  len(sb.buffer),
		HasSnapshot: sb.hasSnap,
		Levels:      len(sb.bids) + len(sb.asks),
	}
}
