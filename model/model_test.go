package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDurationMSJSON(t *testing.T) {
	d := DurationMS(2500 * time.Millisecond)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "2500" {
		t.Fatalf("marshalled = %s", b)
	}
	var got DurationMS
	if err := json.Unmarshal([]byte("3000"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Duration() != 3*time.Second {
		t.Fatalf("unmarshalled = %v", got.Duration())
	}
}

func TestDepthViewHelpers(t *testing.T) {
	v := DepthView{
		Bids: []PriceLevel{{Price: 99, Size: 1}, {Price: 98, Size: 1}},
		Asks: []PriceLevel{{Price: 101, Size: 1}, {Price: 102, Size: 1}},
	}
	if v.MidPrice() != 100 || v.Spread() != 2 {
		t.Fatalf("mid/spread = %v/%v", v.MidPrice(), v.Spread())
	}
	if v.BestPrice(SideBuy) != 101 || v.BestPrice(SideSell) != 99 {
		t.Fatalf("best prices wrong")
	}
	oneSided := DepthView{Bids: []PriceLevel{{Price: 99, Size: 1}}}
	if oneSided.MidPrice() != 0 || oneSided.BestPrice(SideBuy) != 0 {
		t.Fatal("one-sided book must report zero (unavailable), not a fabricated price")
	}
}
