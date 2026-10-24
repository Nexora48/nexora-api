package amenity

import (
	"testing"
	"time"
)

func amenityOpen(slot int, opens, closes string) *Amenity {
	a := &Amenity{Capacity: 1, SlotMinutes: slot}
	for d := 0; d < 7; d++ {
		a.Hours = append(a.Hours, Hours{Weekday: d, Opens: opens, Closes: closes})
	}
	return a
}

func TestGridUsesLocalWallClock(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	d := time.Date(2026, 11, 2, 0, 0, 0, 0, loc)
	s := grid(amenityOpen(60, "06:00", "22:00"), d, loc)
	if len(s) != 16 || s[0].Start.In(loc).Hour() != 6 || s[15].End.In(loc).Hour() != 22 {
		t.Fatalf("slots: %d", len(s))
	}
	if odd := grid(amenityOpen(90, "06:00", "09:30"), d, loc); len(odd) != 2 { // 06:00-07:30, 07:30-09:00; 09:00-10:30 does not fit
		t.Fatalf("90-minute slots: %d", len(odd))
	}
	if none := grid(&Amenity{SlotMinutes: 60}, d, loc); len(none) != 0 {
		t.Fatal("a day without hours must be closed")
	}
}

func TestGridAcrossDaylightSaving(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	a := amenityOpen(60, "00:00", "06:00")
	spring := grid(a, time.Date(2026, 3, 8, 0, 0, 0, 0, loc), loc) // 02:00 does not exist
	seen := map[time.Time]bool{}
	for _, s := range spring {
		if seen[s.Start] || !s.End.After(s.Start) {
			t.Fatalf("duplicate or empty slot at %v", s.Start)
		}
		seen[s.Start] = true
	}
	if len(spring) != 5 {
		t.Fatalf("spring forward: %d slots, want 5", len(spring))
	}
	fall := grid(a, time.Date(2026, 11, 1, 0, 0, 0, 0, loc), loc) // 01:00 happens twice
	seen = map[time.Time]bool{}
	for _, s := range fall {
		if seen[s.Start] {
			t.Fatalf("duplicate slot at %v", s.Start)
		}
		seen[s.Start] = true
	}
	if len(fall) != 6 {
		t.Fatalf("fall back: %d slots, want 6", len(fall))
	}
}
