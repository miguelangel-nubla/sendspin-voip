package sendspin

import (
	"encoding/binary"
	"testing"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

func TestDecodePCM_16Bit(t *testing.T) {
	// 4 samples of 16-bit little-endian
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint16(raw[0:2], uint16(0))
	binary.LittleEndian.PutUint16(raw[2:4], uint16(1000))
	var negVal int16 = -1000
	binary.LittleEndian.PutUint16(raw[4:6], uint16(negVal))
	binary.LittleEndian.PutUint16(raw[6:8], uint16(32767))

	samples := decodePCM(raw, 16)
	if len(samples) != 4 {
		t.Fatalf("expected 4 samples, got %d", len(samples))
	}
	if samples[0] != 0 || samples[1] != 1000 || samples[2] != -1000 || samples[3] != 32767 {
		t.Errorf("unexpected 16-bit decoded samples: %v", samples)
	}
}

func TestDecodePCM_24Bit(t *testing.T) {
	// 2 samples of 24-bit little-endian:
	// Sample 1: 0x00, 0x80, 0x00 -> 0x008000 (32768 in 24-bit) -> scaled >> 8 = 128
	// Sample 2: 0xFF, 0x7F, 0x7F -> 0x7F7FFF (~8355839 in 24-bit) -> scaled >> 8 = 32639
	raw := []byte{
		0x00, 0x80, 0x00,
		0xFF, 0x7F, 0x7F,
	}

	samples := decodePCM(raw, 24)
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}
	if samples[0] != 128 {
		t.Errorf("expected sample 0 to be 128, got %d", samples[0])
	}
	if samples[1] != 32639 {
		t.Errorf("expected sample 1 to be 32639, got %d", samples[1])
	}
}

func TestNormalizeChannels(t *testing.T) {
	tests := []struct {
		name     string
		channels int
		fallback int
		want     int
	}{
		{"mono passes through", 1, 2, 1},
		{"stereo passes through", 2, 1, 2},
		{"zero falls back", 0, 1, 1},
		{"negative falls back", -3, 2, 2},
		// A server announcing a layout the Opus decoder was not built for used to
		// be trusted verbatim, which sized the sample slice past the decode buffer
		// and panicked with an index-out-of-range on the first chunk.
		{"unsupported layout falls back", 6, 2, 2},
		{"unsupported layout with bad fallback defaults to stereo", 6, 0, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.NormalizeChannels(tt.channels, tt.fallback); got != tt.want {
				t.Errorf("NormalizeChannels(%d, %d) = %d, want %d", tt.channels, tt.fallback, got, tt.want)
			}
		})
	}
}
