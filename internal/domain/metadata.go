package domain

import (
	"fmt"
	"time"
)

// StreamMetadata captures track and stream information received from Sendspin / Music Assistant.
type StreamMetadata struct {
	Title           string
	Artist          string
	AlbumArtist     string
	Album           string
	MediaType       string
	Duration        time.Duration
	ProgressMs      int
	ProgressUpdated time.Time
	StreamTitle     string
}

// TrackDisplay returns a formatted track title (e.g. "Artist - Title" or "Title").
func (m StreamMetadata) TrackDisplay() string {
	if m.Title == "" {
		return ""
	}
	if m.Artist != "" {
		return fmt.Sprintf("%s - %s", m.Artist, m.Title)
	}
	return m.Title
}

// ElapsedSeconds calculates real-time playhead progress based on last progress update and playback state.
func (m StreamMetadata) ElapsedSeconds(isPlaying bool) float64 {
	progSec := float64(m.ProgressMs) / 1000.0
	if isPlaying && !m.ProgressUpdated.IsZero() {
		progSec += time.Since(m.ProgressUpdated).Seconds()
		if m.Duration > 0 && progSec > m.Duration.Seconds() {
			progSec = m.Duration.Seconds()
		}
	}
	if progSec < 0 {
		progSec = 0
	}
	return progSec
}
