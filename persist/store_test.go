package persist

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/guoyyit/execution-cost-estimator-go/model"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)
	st := &State{
		DepthPositions: map[string]DepthPosition{
			"BTC": {Symbol: "BTC", LastSeq: 99, Stale: false},
		},
		Plans: []*model.Plan{{
			ID: "plan-1", Symbol: "BTC", Status: model.PlanActive,
			ArrivalMid: 100, CreatedAt: time.Now(),
			Slices: []model.Slice{{Index: 0, Status: model.SliceSettled, Quantity: 5, SettledAt: time.Now()}},
		}},
	}
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	if st.Version != 1 {
		t.Fatalf("version = %d", st.Version)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.DepthPositions["BTC"].LastSeq != 99 {
		t.Fatalf("position = %+v", got.DepthPositions["BTC"])
	}
	if len(got.Plans) != 1 || got.Plans[0].ID != "plan-1" || got.Plans[0].Slices[0].Quantity != 5 {
		t.Fatalf("plans not restored: %+v", got.Plans)
	}
	if got.Plans[0].Slices[0].Status != model.SliceSettled {
		t.Fatalf("settled state lost")
	}
	// No temp file left behind.
	if _, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); err == nil {
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp"))
		if len(matches) != 0 {
			t.Fatalf("temp files left: %v", matches)
		}
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "absent.json"))
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Plans) != 0 || len(st.DepthPositions) != 0 {
		t.Fatal("missing file should yield empty state")
	}
}

func TestVersionMonotonic(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "s.json"))
	st := &State{}
	for i := 1; i <= 3; i++ {
		if err := s.Save(st); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Load()
	if got.Version != 3 {
		t.Fatalf("version = %d, want 3", got.Version)
	}
}
