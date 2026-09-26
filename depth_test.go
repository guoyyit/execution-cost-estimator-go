package estimator

import (
	"errors"
	"testing"
	"time"
)

func mkLevels(prices ...[2]float64) []PriceLevel {
	out := make([]PriceLevel, 0, len(prices))
	for _, p := range prices {
		out = append(out, PriceLevel{Price: p[0], Size: p[1]})
	}
	return out
}

func sampleSnapshot(seq uint64) DepthUpdate {
	return DepthUpdate{
		Symbol:     "BTC-USD",
		Seq:        seq,
		Snapshot:   true,
		Bids:       mkLevels([2]float64{99, 5}, [2]float64{98, 8}),
		Asks:       mkLevels([2]float64{101, 5}, [2]float64{102, 8}),
		ReceivedAt: time.Unix(1000, 0),
	}
}

// Full-rebuild reference: applying an ordered sequence to a fresh book.
func rebuild(updates []DepthUpdate) *depthBook {
	b := newDepthBook("BTC-USD")
	for _, u := range updates {
		b.apply(u)
	}
	return b
}

func TestOutOfOrderIncrementsDrainInOrder(t *testing.T) {
	snap := sampleSnapshot(10)
	u11 := DepthUpdate{Symbol: "BTC-USD", Seq: 11, Bids: mkLevels([2]float64{100, 3}), Asks: mkLevels([2]float64{101, 7})}
	u12 := DepthUpdate{Symbol: "BTC-USD", Seq: 12, Bids: mkLevels([2]float64{99, 0})}
	u13 := DepthUpdate{Symbol: "BTC-USD", Seq: 13, Asks: mkLevels([2]float64{102, 0})}

	b := newDepthBook("BTC-USD")
	b.apply(snap)
	// Arrival order 13, 12, 11 must yield the same book as ordered replay.
	b.apply(u13)
	if b.lastSeq != 10 {
		t.Fatalf("gap must freeze book at snapshot seq, got %d", b.lastSeq)
	}
	b.apply(u12)
	b.apply(u11)
	if b.lastSeq != 13 {
		t.Fatalf("contiguous run must drain through, got %d", b.lastSeq)
	}

	ref := rebuild([]DepthUpdate{snap, u11, u12, u13})
	s1, _ := b.snapshot(time.Now())
	s2, _ := ref.snapshot(time.Now())
	if len(s1.Bids) != len(s2.Bids) || len(s1.Asks) != len(s2.Asks) {
		t.Fatalf("out-of-order book %v/%v != rebuild %v/%v", s1.Bids, s1.Asks, s2.Bids, s2.Asks)
	}
	if s1.Mid != s2.Mid || s1.Seq != s2.Seq {
		t.Fatalf("out-of-order result diverged from full rebuild: %+v vs %+v", s1, s2)
	}
}

func TestDuplicateAndLateIncrementsIgnored(t *testing.T) {
	b := newDepthBook("BTC-USD")
	b.apply(sampleSnapshot(10))
	u11 := DepthUpdate{Symbol: "BTC-USD", Seq: 11, Bids: mkLevels([2]float64{100, 3})}
	if !b.apply(u11) {
		t.Fatal("first application should be applied")
	}
	if b.apply(u11) {
		t.Fatal("duplicate seq must be ignored")
	}
	if b.apply(DepthUpdate{Symbol: "BTC-USD", Seq: 5, Bids: mkLevels([2]float64{1, 1})}) {
		t.Fatal("late increment must be ignored")
	}
	if _, ok := b.bids[1]; ok {
		t.Fatal("late message must not modify the book")
	}
	if b.lastSeq != 11 {
		t.Fatalf("seq must stay 11, got %d", b.lastSeq)
	}
}

func TestMissingIncrementHoldsUntilSnapshot(t *testing.T) {
	b := newDepthBook("BTC-USD")
	b.apply(sampleSnapshot(10))
	// 11 never arrives; 12 and 13 are buffered, book stays at seq 10.
	b.apply(DepthUpdate{Symbol: "BTC-USD", Seq: 12, Bids: mkLevels([2]float64{100, 9})})
	b.apply(DepthUpdate{Symbol: "BTC-USD", Seq: 13, Bids: mkLevels([2]float64{97, 2})})
	if b.lastSeq != 10 || !b.hasGap() {
		t.Fatalf("missing predecessor must freeze book, seq=%d gap=%v", b.lastSeq, b.hasGap())
	}
	// Next snapshot re-anchors; stale buffered increments must not replay.
	b.apply(sampleSnapshot(20))
	if b.lastSeq != 20 || len(b.pending) != 0 {
		t.Fatalf("snapshot must reset seq and buffer, seq=%d pending=%d", b.lastSeq, len(b.pending))
	}
	if _, ok := b.bids[100]; ok {
		t.Fatal("pre-snapshot buffered updates must not be applied after re-anchor")
	}
}

func TestEmptyAndOneSidedBookRejected(t *testing.T) {
	b := newDepthBook("BTC-USD")
	if _, ok := b.snapshot(time.Now()); ok {
		t.Fatal("never-anchored book must not snapshot")
	}
	b.apply(DepthUpdate{Symbol: "BTC-USD", Seq: 1, Snapshot: true})
	if b.complete() {
		t.Fatal("empty snapshot must not be considered complete")
	}
	b2 := newDepthBook("BTC-USD")
	b2.apply(DepthUpdate{Symbol: "BTC-USD", Seq: 1, Snapshot: true, Bids: mkLevels([2]float64{100, 1})})
	if b2.complete() {
		t.Fatal("one-sided book must not be considered complete")
	}
}

func TestWalkInsufficientDepth(t *testing.T) {
	b := newDepthBook("BTC-USD")
	b.apply(sampleSnapshot(1))
	snap, _ := b.snapshot(time.Now())
	tiers, ok := walk(Buy, snap, 20)
	if ok {
		t.Fatalf("ask depth 13 cannot fill 20")
	}
	if len(tiers) != 2 {
		t.Fatalf("should still expose walked tiers, got %d", len(tiers))
	}
	if _, err := avgFillPrice(Buy, snap, 20); !errors.Is(err, ErrDepthInsufficient) {
		t.Fatalf("want ErrDepthInsufficient, got %v", err)
	}
	if _, err := avgFillPrice(Buy, snap, 10); err != nil {
		t.Fatalf("10 fits within 13 ask depth, got %v", err)
	}
}

func TestSnapshotIsCopied(t *testing.T) {
	b := newDepthBook("BTC-USD")
	b.apply(sampleSnapshot(1))
	snap, _ := b.snapshot(time.Now())
	snap.Bids[0].Size = 999
	if b.bids[99] == 999 {
		t.Fatal("snapshot must be a defensive copy")
	}
}
