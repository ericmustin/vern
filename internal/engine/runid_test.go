package engine

import (
	"regexp"
	"testing"
	"time"
)

func TestNewRunID_FormatAndRoundTrip(t *testing.T) {
	now := time.Now()
	id := NewRunID(now)
	if !regexp.MustCompile(`^vern[0-9a-z]+[0-9a-z]{8}$`).MatchString(string(id)) {
		t.Fatalf("bad format: %q", id)
	}
	got, ok := ParseRunID(string(id))
	if !ok || got != id {
		t.Fatalf("ParseRunID(%q) = %q, %v", id, got, ok)
	}
	if want := now.Truncate(time.Second); !id.CreatedAt().Equal(want) {
		t.Fatalf("CreatedAt = %v, want %v", id.CreatedAt(), want)
	}
	if NewRunID(now) == id {
		t.Fatal("two run IDs collided")
	}
}

func TestParseRunID_Rejects(t *testing.T) {
	for _, s := range []string{"", "vernABC12345678", "other1234567890", "vern12345678", "vern", "vern-abc12345678", "vernzzzzzzzzzzzzzzzzzzzzzzzz"} {
		if _, ok := ParseRunID(s); ok {
			t.Errorf("ParseRunID(%q) accepted", s)
		}
	}
}

func TestLoadStats_Dropped(t *testing.T) {
	s := LoadStats{
		Sent:    map[string]int{"traces": 10, "metrics": 5, "logs": 4, "x": 2},
		Indexed: map[string]int{"traces": 8, "metrics": -1, "logs": 4, "x": 2},
		Failed:  map[string]int{"traces": 1, "metrics": 3},
	}
	d := s.Dropped()
	if d["traces"] != 2 || d["metrics"] != 3 {
		t.Fatalf("got %v", d)
	}
	if _, ok := d["logs"]; ok {
		t.Fatalf("zero not omitted: %v", d)
	}
	if _, ok := d["x"]; ok {
		t.Fatalf("zero not omitted: %v", d)
	}
}
