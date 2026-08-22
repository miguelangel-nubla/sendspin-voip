package rtp

import (
	"testing"
)

func TestParseDTMFPayload(t *testing.T) {
	// Digit '5', End bit set (0x80), volume 10, duration 160
	payload := []byte{5, 0x80 | 10, 0x00, 0xA0}

	evt, err := ParseDTMFPayload(payload)
	if err != nil {
		t.Fatalf("ParseDTMFPayload failed: %v", err)
	}
	if evt.Digit != "5" {
		t.Errorf("expected digit '5', got %s", evt.Digit)
	}
	if !evt.EndOfEvt {
		t.Errorf("expected EndOfEvt to be true")
	}
	if evt.Volume != 10 {
		t.Errorf("expected volume 10, got %d", evt.Volume)
	}
	if evt.Duration != 160 {
		t.Errorf("expected duration 160, got %d", evt.Duration)
	}
}

func TestEventToDigit_SpecialKeys(t *testing.T) {
	tests := []struct {
		event byte
		want  string
	}{
		{10, "*"},
		{11, "#"},
		{12, "A"},
		{13, "B"},
		{14, "C"},
		{15, "D"},
		{16, "FLASH"},
	}

	for _, tt := range tests {
		got, ok := EventToDigit(tt.event)
		if !ok || got != tt.want {
			t.Errorf("EventToDigit(%d) = (%s, %v), want (%s, true)", tt.event, got, ok, tt.want)
		}
	}
}
