package estimator

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreRoundTripAndMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.json")
	store := NewFileStore(path)

	// Missing file -> empty state, no error.
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Depth.Valid || len(st.Plans) != 0 {
		t.Fatal("missing store must load empty")
	}

	b := newDepthBook("BTC-USD")
	b.apply(DepthUpdate{
		Symbol: "BTC-USD", Seq: 3, Snapshot: true,
		Bids:       mkLevels([2]float64{10, 2}),
		Asks:       mkLevels([2]float64{11, 2}),
		ReceivedAt: time.Unix(123, 0),
	})
	want := PersistedState{Depth: b.toPersisted(), Plans: []persistedPlan{{ID: "p1", Status: PlanRunning}}}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Depth.LastSeq != 3 || !got.Depth.Valid || len(got.Depth.Bids) != 1 || got.Plans[0].ID != "p1" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
