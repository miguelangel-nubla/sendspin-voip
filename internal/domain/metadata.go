package domain

import (
	"time"
)

// StreamMetadata captures track and stream information received from Sendspin / Music Assistant.
type StreamMetadata struct {
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	MediaType   string
	Duration    time.Duration
	StreamTitle string
}

