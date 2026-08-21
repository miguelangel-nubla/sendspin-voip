package sip

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	"github.com/pion/sdp/v3"
)

// BuildSDPOffer creates an SDP offer string for the specified codec and local RTP port.
// An empty preferred codec means "auto": offer DefaultCodecPreferences in fidelity order.
func BuildSDPOffer(localIP string, rtpPort int, codec domain.Codec) (string, error) {
	if localIP == "" {
		localIP = "127.0.0.1"
	}

	sessionID := uint64(time.Now().UnixNano())
	var randBuf [8]byte
	if _, err := rand.Read(randBuf[:]); err == nil {
		sessionID = binary.BigEndian.Uint64(randBuf[:]) & 0x7fffffffffffffff
	}

	sd := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      sessionID,
			SessionVersion: 1,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: localIP,
		},
		SessionName: "sendspin-voip",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: localIP},
		},
		TimeDescriptions: []sdp.TimeDescription{
			{Timing: sdp.Timing{StartTime: 0, StopTime: 0}},
		},
	}

	// Build ordered codec list with preferred codec first (only if listed), followed by fallbacks
	allCodecs := domain.PrioritizeCodecs(codec, nil)

	var formats []string
	var rtpmapAttrs []sdp.Attribute
	for _, c := range allCodecs {
		ptStr := strconv.Itoa(int(c.PayloadType()))
		formats = append(formats, ptStr)
		switch c {
		case domain.CodecPCMU:
			rtpmapAttrs = append(rtpmapAttrs, sdp.NewAttribute("rtpmap", "0 PCMU/8000"))
		case domain.CodecPCMA:
			rtpmapAttrs = append(rtpmapAttrs, sdp.NewAttribute("rtpmap", "8 PCMA/8000"))
		case domain.CodecG722:
			rtpmapAttrs = append(rtpmapAttrs, sdp.NewAttribute("rtpmap", "9 G722/8000"))
		case domain.CodecOpus:
			rtpmapAttrs = append(rtpmapAttrs, sdp.NewAttribute("rtpmap", "96 opus/48000/2"))
		case domain.CodecL16:
			// Match transcoder output: mono 48 kHz L16 (RFC 3551 uses big-endian PCM)
			rtpmapAttrs = append(rtpmapAttrs, sdp.NewAttribute("rtpmap", "97 L16/48000/1"))
		}
	}

	media := sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "audio",
			Port:    sdp.RangedPort{Value: rtpPort},
			Protos:  []string{"RTP", "AVP"},
			Formats: formats,
		},
		Attributes: append([]sdp.Attribute{
			sdp.NewPropertyAttribute("sendrecv"),
		}, rtpmapAttrs...),
	}

	sd.MediaDescriptions = append(sd.MediaDescriptions, &media)

	bytes, err := sd.Marshal()
	if err != nil {
		return "", fmt.Errorf("failed to marshal SDP: %w", err)
	}

	return string(bytes), nil
}

// ParseRemoteSDP parses the remote 200 OK SDP answer to extract the remote RTP UDP address.
func ParseRemoteSDP(sdpRaw string, fallbackHost string) (*net.UDPAddr, domain.Codec, error) {
	sd := &sdp.SessionDescription{}
	if err := sd.Unmarshal([]byte(sdpRaw)); err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal remote SDP: %w", err)
	}

	// 1. Determine Remote IP
	remoteIP := fallbackHost
	if sd.ConnectionInformation != nil && sd.ConnectionInformation.Address != nil {
		remoteIP = sd.ConnectionInformation.Address.Address
	}

	// 2. Determine Remote Port and Codec from Media Descriptions
	var remotePort int
	selectedCodec := domain.CodecPCMU

	for _, m := range sd.MediaDescriptions {
		if strings.EqualFold(m.MediaName.Media, "audio") {
			remotePort = m.MediaName.Port.Value

			// If media description overrides connection info
			if m.ConnectionInformation != nil && m.ConnectionInformation.Address != nil {
				remoteIP = m.ConnectionInformation.Address.Address
			}

			ptToCodec := map[uint8]domain.Codec{}
			for _, attr := range m.Attributes {
				if !strings.EqualFold(attr.Key, "rtpmap") {
					continue
				}
				pt, codec, ok := parseRtpmap(attr.Value)
				if ok {
					ptToCodec[pt] = codec
				}
			}

			// Answer should list negotiated format(s); prefer first with known mapping
			for _, fmtStr := range m.MediaName.Formats {
				pt64, err := strconv.ParseUint(fmtStr, 10, 8)
				if err != nil {
					continue
				}
				pt := uint8(pt64)
				if c, ok := ptToCodec[pt]; ok {
					selectedCodec = c
					break
				}
				// Static payload types (RFC 3551) when rtpmap omitted
				switch pt {
				case 0:
					selectedCodec = domain.CodecPCMU
				case 8:
					selectedCodec = domain.CodecPCMA
				case 9:
					selectedCodec = domain.CodecG722
				case 10, 11:
					selectedCodec = domain.CodecL16
				default:
					continue
				}
				break
			}
			break
		}
	}

	if remotePort == 0 {
		return nil, "", fmt.Errorf("no audio media port found in remote SDP")
	}

	ip := net.ParseIP(remoteIP)
	if ip == nil {
		// Try resolving DNS if it's a hostname
		ips, err := net.LookupIP(remoteIP)
		if err != nil || len(ips) == 0 {
			return nil, "", fmt.Errorf("invalid or unresolvable remote RTP IP: %s", remoteIP)
		}
		ip = ips[0]
	}

	return &net.UDPAddr{IP: ip, Port: remotePort}, selectedCodec, nil
}

// ParseSDPCodecs extracts all supported audio codecs present in an SDP description.
// Dynamic payload types are only mapped when an rtpmap attribute identifies them.
func ParseSDPCodecs(sdpRaw string) []domain.Codec {
	sd := &sdp.SessionDescription{}
	if err := sd.Unmarshal([]byte(sdpRaw)); err != nil {
		return nil
	}

	var codecs []domain.Codec
	seen := make(map[domain.Codec]bool)

	add := func(c domain.Codec) {
		if c == "" || seen[c] {
			return
		}
		codecs = append(codecs, c)
		seen[c] = true
	}

	for _, m := range sd.MediaDescriptions {
		if !strings.EqualFold(m.MediaName.Media, "audio") {
			continue
		}

		ptToCodec := map[uint8]domain.Codec{}
		for _, attr := range m.Attributes {
			if !strings.EqualFold(attr.Key, "rtpmap") {
				continue
			}
			pt, codec, ok := parseRtpmap(attr.Value)
			if ok {
				ptToCodec[pt] = codec
				add(codec)
			}
		}

		// Static PTs from m= line when not already covered by rtpmap
		for _, fmtStr := range m.MediaName.Formats {
			pt64, err := strconv.ParseUint(fmtStr, 10, 8)
			if err != nil {
				continue
			}
			pt := uint8(pt64)
			if _, ok := ptToCodec[pt]; ok {
				continue
			}
			switch pt {
			case 0:
				add(domain.CodecPCMU)
			case 8:
				add(domain.CodecPCMA)
			case 9:
				add(domain.CodecG722)
			case 10, 11:
				add(domain.CodecL16)
			}
		}
	}
	return codecs
}

func parseRtpmap(value string) (pt uint8, codec domain.Codec, ok bool) {
	val := strings.ToLower(strings.TrimSpace(value))
	parts := strings.Fields(val)
	if len(parts) < 2 {
		return 0, "", false
	}
	pt64, err := strconv.ParseUint(parts[0], 10, 8)
	if err != nil {
		return 0, "", false
	}
	name := parts[1]
	switch {
	case strings.HasPrefix(name, "opus"):
		return uint8(pt64), domain.CodecOpus, true
	case strings.HasPrefix(name, "l16") || strings.HasPrefix(name, "linear16"):
		return uint8(pt64), domain.CodecL16, true
	case strings.HasPrefix(name, "g722"):
		return uint8(pt64), domain.CodecG722, true
	case strings.HasPrefix(name, "pcmu") || strings.HasPrefix(name, "ulaw"):
		return uint8(pt64), domain.CodecPCMU, true
	case strings.HasPrefix(name, "pcma") || strings.HasPrefix(name, "alaw"):
		return uint8(pt64), domain.CodecPCMA, true
	default:
		return uint8(pt64), "", false
	}
}
