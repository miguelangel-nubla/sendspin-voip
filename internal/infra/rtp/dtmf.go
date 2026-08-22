package rtp

import (
	"encoding/binary"
	"fmt"
)

// DTMFEvent represents a decoded RFC 2833 / RFC 4733 telephone-event.
type DTMFEvent struct {
	Digit    string // "0"-"9", "*", "#", "A"-"D", "FLASH"
	Event    byte   // Raw event ID (0-16)
	EndOfEvt bool   // True if the 'E' bit is set
	Volume   byte   // 0-63
	Duration uint16 // Duration in timestamp units
}

// EventToDigit maps RFC 2833 event codes to their character representations.
func EventToDigit(event byte) (string, bool) {
	switch {
	case event >= 0 && event <= 9:
		return fmt.Sprintf("%d", event), true
	case event == 10:
		return "*", true
	case event == 11:
		return "#", true
	case event == 12:
		return "A", true
	case event == 13:
		return "B", true
	case event == 14:
		return "C", true
	case event == 15:
		return "D", true
	case event == 16:
		return "FLASH", true
	default:
		return "", false
	}
}

// ParseDTMFPayload decodes an RFC 2833 / RFC 4733 telephone-event payload (4 bytes).
func ParseDTMFPayload(payload []byte) (*DTMFEvent, error) {
	if len(payload) < 4 {
		return nil, fmt.Errorf("payload length %d too short for DTMF event", len(payload))
	}

	eventCode := payload[0]
	endOfEvt := (payload[1] & 0x80) != 0
	volume := payload[1] & 0x3F
	duration := binary.BigEndian.Uint16(payload[2:4])

	digit, ok := EventToDigit(eventCode)
	if !ok {
		return nil, fmt.Errorf("unrecognized RFC 4733 event code: %d", eventCode)
	}

	return &DTMFEvent{
		Digit:    digit,
		Event:    eventCode,
		EndOfEvt: endOfEvt,
		Volume:   volume,
		Duration: duration,
	}, nil
}
