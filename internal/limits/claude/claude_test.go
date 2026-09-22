package claude

import (
	"os"
	"testing"
)

func TestParseLimitsArray(t *testing.T) {
	body, err := os.ReadFile("testdata/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.windows) != 3 {
		t.Fatalf("want 3 windows, got %d: %+v", len(p.windows), p.windows)
	}
	if p.windows[0].Key != "five_hour" || p.windows[0].Utilization != 30 || p.windows[0].WindowMinutes != 300 {
		t.Errorf("five_hour: %+v", p.windows[0])
	}
	if p.windows[1].Key != "seven_day" || p.windows[1].Utilization != 42 || p.windows[1].StartedAt.IsZero() {
		t.Errorf("seven_day: %+v", p.windows[1])
	}
	if p.windows[2].Scope != "Fable" || p.windows[2].Utilization != 76 || !p.windows[2].Active {
		t.Errorf("scoped: %+v", p.windows[2])
	}
	if len(p.shares) != 4 || p.shares[0].Label != "Claude Code" || p.shares[0].Percent != 97 {
		t.Errorf("shares: %+v", p.shares)
	}
	if len(p.notes) != 1 {
		t.Errorf("notes: %+v", p.notes)
	}
}

func TestParseFlatOnly(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":12.5,"resets_at":"2026-09-22T21:40:00+00:00"},"seven_day":{"utilization":40,"resets_at":"2026-09-28T01:00:00+00:00"},"seven_day_opus":{"utilization":5,"resets_at":"2026-09-28T01:00:00+00:00"},"nimbus_quill":{"utilization":0,"resets_at":null}}`)
	p, err := parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.windows) != 3 {
		t.Fatalf("want 3 windows, got %+v", p.windows)
	}
	if p.windows[0].Key != "five_hour" || p.windows[1].Key != "seven_day" || p.windows[2].Scope != "opus" {
		t.Errorf("order/scope: %+v", p.windows)
	}
}
