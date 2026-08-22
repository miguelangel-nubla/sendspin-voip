package domain

import (
	"testing"
)

func TestPrioritizeCodecs_DoesNotInjectUndiscovered(t *testing.T) {
	available := []Codec{CodecG722, CodecPCMU}
	got := PrioritizeCodecs(CodecOpus, available)
	if len(got) != 2 || got[0] != CodecG722 || got[1] != CodecPCMU {
		t.Fatalf("expected [g722 pcmu] without injecting opus, got %v", got)
	}
}

func TestPrioritizeCodecs_MovesPreferredWhenPresent(t *testing.T) {
	available := []Codec{CodecOpus, CodecG722, CodecPCMU}
	got := PrioritizeCodecs(CodecG722, available)
	if len(got) != 3 || got[0] != CodecG722 || got[1] != CodecOpus || got[2] != CodecPCMU {
		t.Fatalf("expected g722 first, got %v", got)
	}
}

func TestPrioritizeCodecs_EmptyPreferredUsesAvailableOrder(t *testing.T) {
	available := []Codec{CodecG722, CodecPCMU}
	got := PrioritizeCodecs("", available)
	if len(got) != 2 || got[0] != CodecG722 {
		t.Fatalf("expected discovery order preserved, got %v", got)
	}
}

func TestPrioritizeCodecs_NilAvailableUsesDefaults(t *testing.T) {
	got := PrioritizeCodecs(CodecG722, nil)
	if got[0] != CodecG722 {
		t.Fatalf("expected g722 first among defaults, got %v", got)
	}
	if len(got) != len(DefaultCodecPreferences) {
		t.Fatalf("expected %d codecs, got %d", len(DefaultCodecPreferences), len(got))
	}
}

func TestParseCodec_AutoIsEmpty(t *testing.T) {
	for _, in := range []string{"", "auto", "best", "AUTO"} {
		c, err := ParseCodec(in)
		if err != nil {
			t.Fatalf("ParseCodec(%q): %v", in, err)
		}
		if c != "" {
			t.Fatalf("ParseCodec(%q) = %q, want empty (auto)", in, c)
		}
	}
}

func TestNormalizeSIPTarget(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"sip:101@192.168.1.50", "sip:101@192.168.1.50"},
		{"SIP:101@192.168.1.50", "sip:101@192.168.1.50"},
		{"101@192.168.1.50", "sip:101@192.168.1.50"},
		{"<sip:101@host>", "sip:101@host"},
		{"sips:101@host", "sip:101@host"},
		{"  sip:101@Host  ", "sip:101@host"},
	}
	for _, tc := range cases {
		if got := NormalizeSIPTarget(tc.in); got != tc.want {
			t.Errorf("NormalizeSIPTarget(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeChannels(t *testing.T) {
	cases := []struct {
		channels, fallback, want int
	}{
		{1, 2, 1},
		{2, 1, 2},
		{0, 1, 1},
		{0, 2, 2},
		{6, 0, 2},
		{6, 1, 1},
	}
	for _, tc := range cases {
		if got := NormalizeChannels(tc.channels, tc.fallback); got != tc.want {
			t.Errorf("NormalizeChannels(%d, %d) = %d, want %d", tc.channels, tc.fallback, got, tc.want)
		}
	}
}

func TestFormatAudioDescription(t *testing.T) {
	if got := FormatAudioDescription("opus", 48000, 2, 16); got != "OPUS 48000Hz 2ch" {
		t.Errorf("FormatAudioDescription(opus) = %q, want %q", got, "OPUS 48000Hz 2ch")
	}
	if got := FormatAudioDescription("pcm", 44100, 2, 16); got != "PCM 44100Hz 2ch 16bit" {
		t.Errorf("FormatAudioDescription(pcm) = %q, want %q", got, "PCM 44100Hz 2ch 16bit")
	}
}

func TestCalculateBitrateKbps(t *testing.T) {
	if got := CalculateBitrateKbps("opus", 48000, 2, 16); got != 128 {
		t.Errorf("CalculateBitrateKbps(opus) = %d, want 128", got)
	}
	if got := CalculateBitrateKbps("pcm", 48000, 2, 16); got != 1536 {
		t.Errorf("CalculateBitrateKbps(pcm) = %d, want 1536", got)
	}
}
