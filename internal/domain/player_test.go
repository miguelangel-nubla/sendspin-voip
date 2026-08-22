package domain

import (
	"testing"
)

func TestClampVolume(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{-10, 0},
		{0, 0},
		{50, 50},
		{100, 100},
		{150, 100},
	}

	for _, tt := range tests {
		if got := ClampVolume(tt.in); got != tt.want {
			t.Errorf("ClampVolume(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestPlayer_EffectiveVolume(t *testing.T) {
	p := &Player{Volume: 75, IsMuted: false}
	if got := p.EffectiveVolume(); got != 75 {
		t.Errorf("EffectiveVolume() = %d, want 75", got)
	}

	p.IsMuted = true
	if got := p.EffectiveVolume(); got != 0 {
		t.Errorf("EffectiveVolume() when muted = %d, want 0", got)
	}
}

func TestPlayer_SetVolume(t *testing.T) {
	p := &Player{Volume: 50}
	p.SetVolume(120)
	if p.Volume != 100 {
		t.Errorf("SetVolume(120) = %d, want 100", p.Volume)
	}
	p.SetVolume(-5)
	if p.Volume != 0 {
		t.Errorf("SetVolume(-5) = %d, want 0", p.Volume)
	}
}

func TestFormatVolumeGain(t *testing.T) {
	tests := []struct {
		vol   int
		muted bool
		want  string
	}{
		{0, false, "volume mute"},
		{50, true, "volume mute"},
		{100, false, "volume 100% (0 dB)"},
		{50, false, "volume 50% (-30.0 dB)"},
	}

	for _, tt := range tests {
		got := FormatVolumeGain(tt.vol, tt.muted)
		if got != tt.want {
			t.Errorf("FormatVolumeGain(%d, %v) = %q, want %q", tt.vol, tt.muted, got, tt.want)
		}
	}
}
