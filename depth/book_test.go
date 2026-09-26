package depth

import (
	"errors"
	"testing"

	"github.com/guoyyit/execution-cost-estimator-go/model"
)

func mkLevels(base float64, n int, size float64, bid bool) []model.PriceLevel {
	out := make([]model.PriceLevel, n)
	for i := 0; i < n; i++ {
		p := base + float64(i)
		if bid {
			p = base - float64(i)
		}
		out[i] = model.PriceLevel{Price: p, Size: size}
	}
	return out
}

func snap(seq int64) model.Snapshot {
	return model.Snapshot{View: model.DepthView{
		Symbol: "BTC",
		Seq:    seq,
		Bids:   []model.PriceLevel{{Price: 99, Size: 5}, {Price: 98, Size: 7}},
		Asks:   []model.PriceLevel{{Price: 101, Size: 5}, {Price: 102, Size: 7}},
	}}
}

func upd(seq int64, bids, asks []model.PriceLevel) model.DepthUpdate {
	return model.DepthUpdate{Symbol: "BTC", Seq: seq, Bids: bids, Asks: asks}
}

// Applying updates in order and out of order must yield the same book.
func TestOutOfOrderEqualsInOrder(t *testing.T) {
	inOrder := NewBook()
	if err := inOrder.Reset(snap(1)); err != nil {
		t.Fatal(err)
	}
	updates := []model.DepthUpdate{
		upd(2, []model.PriceLevel{{Price: 99, Size: 10}}, nil),
		upd(3, nil, []model.PriceLevel{{Price: 103, Size: 3}}),
		upd(4, []model.PriceLevel{{Price: 98, Size: 0}}, nil), // delete
		upd(5, nil, []model.PriceLevel{{Price: 101, Size: 8}}),
	}
	for _, u := range updates {
		if err := inOrder.Apply(u); err != nil {
			t.Fatal(err)
		}
	}

	reordered := NewBook()
	if err := reordered.Reset(snap(1)); err != nil {
		t.Fatal(err)
	}
	shuffled := []model.DepthUpdate{updates[3], updates[1], updates[0], updates[2]}
	for _, u := range shuffled {
		if err := reordered.Apply(u); err != nil {
			t.Fatal(err)
		}
	}

	a, err := inOrder.View("BTC")
	if err != nil {
		t.Fatal(err)
	}
	b, err := reordered.View("BTC")
	if err != nil {
		t.Fatal(err)
	}
	if !viewsEqual(a, b) {
		t.Fatalf("reordered book diverged:\n%+v\n%+v", a, b)
	}
	if a.Seq != 5 || b.Seq != 5 {
		t.Fatalf("seq = %d/%d, want 5", a.Seq, b.Seq)
	}
}

func TestDuplicatesIgnored(t *testing.T) {
	b := NewBook()
	if err := b.Reset(snap(1)); err != nil {
		t.Fatal(err)
	}
	u := upd(2, []model.PriceLevel{{Price: 99, Size: 10}}, nil)
	for i := 0; i < 5; i++ {
		if err := b.Apply(u); err != nil {
			t.Fatal(err)
		}
	}
	v, err := b.View("BTC")
	if err != nil {
		t.Fatal(err)
	}
	if v.Bids[0].Size != 10 || v.Seq != 2 {
		t.Fatalf("duplicate update changed state: %+v", v.Bids[0])
	}
}

func TestGapStaleThenHeals(t *testing.T) {
	b := NewBook()
	if err := b.Reset(snap(1)); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(upd(3, nil, nil)); err != nil { // gap: 2 missing
		t.Fatal(err)
	}
	if _, err := b.View("BTC"); !errors.Is(err, ErrStaleDepth) {
		t.Fatalf("want ErrStaleDepth during gap, got %v", err)
	}
	st := b.StateOf("BTC")
	if !st.Stale || st.BufferSize != 1 || st.LastSeq != 1 {
		t.Fatalf("unexpected state: %+v", st)
	}
	// Late arrival of seq 2 heals the gap and applies 3 contiguously.
	if err := b.Apply(upd(2, []model.PriceLevel{{Price: 99, Size: 11}}, nil)); err != nil {
		t.Fatal(err)
	}
	v, err := b.View("BTC")
	if err != nil {
		t.Fatalf("gap should heal: %v", err)
	}
	if v.Seq != 3 || v.Bids[0].Size != 11 {
		t.Fatalf("healed book wrong: seq=%d bid=%v", v.Seq, v.Bids[0])
	}
	if b.StateOf("BTC").Stale {
		t.Fatal("book must be non-stale after heal")
	}
}

func TestSnapshotHealsGapAndDiscardsOld(t *testing.T) {
	b := NewBook()
	if err := b.Reset(snap(1)); err != nil {
		t.Fatal(err)
	}
	_ = b.Apply(upd(2, []model.PriceLevel{{Price: 99, Size: 10}}, nil))
	_ = b.Apply(upd(4, nil, nil)) // opens gap 3
	if _, err := b.View("BTC"); !errors.Is(err, ErrStaleDepth) {
		t.Fatalf("want stale, got %v", err)
	}
	s := snap(10)
	s.View.Bids[0].Size = 50
	if err := b.Reset(s); err != nil {
		t.Fatal(err)
	}
	v, err := b.View("BTC")
	if err != nil {
		t.Fatalf("snapshot must heal: %v", err)
	}
	if v.Seq != 10 || v.Bids[0].Size != 50 {
		t.Fatalf("snapshot reset wrong: %+v", v)
	}
	// A duplicate of an old committed update is ignored.
	if err := b.Apply(upd(2, []model.PriceLevel{{Price: 99, Size: 999}}, nil)); err != nil {
		t.Fatal(err)
	}
	v, _ = b.View("BTC")
	if v.Bids[0].Size != 50 {
		t.Fatal("old update after snapshot caused drift")
	}
}

func TestEmptyBeforeSnapshot(t *testing.T) {
	b := NewBook()
	if _, err := b.View("X"); !errors.Is(err, model.ErrEmptyDepth) {
		t.Fatalf("want ErrEmptyDepth, got %v", err)
	}
	_ = b.Apply(upd(5, nil, nil)) // future update before any snapshot
	if _, err := b.View("X"); !errors.Is(err, model.ErrEmptyDepth) {
		t.Fatalf("buffered update must not fabricate depth, got %v", err)
	}
}

func TestPrimePositionDropsReplayedUpdates(t *testing.T) {
	b := NewBook()
	b.PrimePosition("BTC", 10)
	// Late/duplicate replay of already committed seqs: must be ignored.
	_ = b.Apply(upd(9, []model.PriceLevel{{Price: 99, Size: 1}}, nil))
	_ = b.Apply(upd(10, []model.PriceLevel{{Price: 99, Size: 1}}, nil))
	if _, err := b.View("BTC"); !errors.Is(err, model.ErrEmptyDepth) {
		t.Fatalf("primed symbol has no levels, want empty, got %v", err)
	}
	st := b.StateOf("BTC")
	if st.LastSeq != 10 || st.HasSnapshot {
		t.Fatalf("prime wrong: %+v", st)
	}
	// A fresh snapshot restores normal operation; buffered future
	// updates (seq 11+) replay automatically.
	_ = b.Apply(upd(12, []model.PriceLevel{{Price: 97, Size: 4}}, nil))
	if err := b.Reset(snap(11)); err != nil {
		t.Fatal(err)
	}
	v, err := b.View("BTC")
	if err != nil {
		t.Fatalf("post snapshot view: %v", err)
	}
	if v.Seq != 12 {
		t.Fatalf("buffered future update should replay to 12, got %d", v.Seq)
	}
}

func viewsEqual(a, b model.DepthView) bool {
	if a.Seq != b.Seq || len(a.Bids) != len(b.Bids) || len(a.Asks) != len(b.Asks) {
		return false
	}
	for i := range a.Bids {
		if a.Bids[i] != b.Bids[i] || a.Asks[i] != b.Asks[i] {
			return false
		}
	}
	return true
}
