package domain

import (
	"testing"
	"time"
)

func TestStreamMetadata_TrackDisplay(t *testing.T) {
	tests := []struct {
		name string
		meta StreamMetadata
		want string
	}{
		{
			name: "empty",
			meta: StreamMetadata{},
			want: "",
		},
		{
			name: "title only",
			meta: StreamMetadata{Title: "Song 1"},
			want: "Song 1",
		},
		{
			name: "artist and title",
			meta: StreamMetadata{Artist: "Queen", Title: "Bohemian Rhapsody"},
			want: "Queen - Bohemian Rhapsody",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.meta.TrackDisplay(); got != tt.want {
				t.Errorf("TrackDisplay() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStreamMetadata_ElapsedSeconds(t *testing.T) {
	now := time.Now()

	meta := StreamMetadata{
		ProgressMs:      10000,
		Duration:        60 * time.Second,
		ProgressUpdated: now.Add(-5 * time.Second),
	}

	// When playing, elapsed should include ~5s since ProgressUpdated
	elapsedPlaying := meta.ElapsedSeconds(true)
	if elapsedPlaying < 14.9 || elapsedPlaying > 16.0 {
		t.Errorf("ElapsedSeconds(true) = %v, want ~15.0", elapsedPlaying)
	}

	// When not playing, elapsed should be fixed at ProgressMs (10.0s)
	elapsedPaused := meta.ElapsedSeconds(false)
	if elapsedPaused != 10.0 {
		t.Errorf("ElapsedSeconds(false) = %v, want 10.0", elapsedPaused)
	}

	// Clamping to duration
	metaClamped := StreamMetadata{
		ProgressMs:      58000,
		Duration:        60 * time.Second,
		ProgressUpdated: now.Add(-5 * time.Second),
	}
	if elapsedClamped := metaClamped.ElapsedSeconds(true); elapsedClamped != 60.0 {
		t.Errorf("ElapsedSeconds(true) clamped = %v, want 60.0", elapsedClamped)
	}
}
