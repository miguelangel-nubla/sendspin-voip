package sip

import (
	"cmp"
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
	localIP = cmp.Or(localIP, "127.0.0.1")

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

	// The RTP pacer emits one packet every 20 ms. Declare that explicitly so a
	// phone that would otherwise assume a different packetization interval sizes
	// its jitter buffer correctly.
	media.Attributes = append(media.Attributes,
		sdp.NewAttribute("ptime", "20"),
		sdp.NewAttribute("maxptime", "20"),
	)

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

			ptToCodec := buildMediaRtpmap(m)

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
				if c, ok := staticRFC3551Codec(pt); ok {
					selectedCodec = c
					break
				}
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

		ptToCodec := buildMediaRtpmap(m)
		for _, fmtStr := range m.MediaName.Formats {
			pt64, err := strconv.ParseUint(fmtStr, 10, 8)
			if err != nil {
				continue
			}
			pt := uint8(pt64)
			if c, ok := ptToCodec[pt]; ok {
				add(c)
			} else if c, ok := staticRFC3551Codec(pt); ok {
				add(c)
			}
		}

		for _, attr := range m.Attributes {
			if strings.EqualFold(attr.Key, "rtpmap") {
				if _, codec, ok := parseRtpmap(attr.Value); ok {
					add(codec)
				}
			}
		}
	}
	return codecs
}

func staticRFC3551Codec(pt uint8) (domain.Codec, bool) {
	switch pt {
	case 0:
		return domain.CodecPCMU, true
	case 8:
		return domain.CodecPCMA, true
	case 9:
		return domain.CodecG722, true
	case 10, 11:
		return domain.CodecL16, true
	default:
		return "", false
	}
}

func buildMediaRtpmap(m *sdp.MediaDescription) map[uint8]domain.Codec {
	ptToCodec := make(map[uint8]domain.Codec)
	for _, attr := range m.Attributes {
		if strings.EqualFold(attr.Key, "rtpmap") {
			if pt, codec, ok := parseRtpmap(attr.Value); ok {
				ptToCodec[pt] = codec
			}
		}
	}
	return ptToCodec
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
	// An rtpmap value looks like "96 opus/48000/2"; the encoding name is the
	// segment before the first "/". Match it exactly rather than by prefix:
	// prefix matching folds G7221 and G722.1 (G.722.1 — a different codec, at a
	// different clock rate and bitrate) into plain G.722, so a phone offering
	// only G.722.1 would be told to expect G.722 and receive unintelligible
	// audio. The same trap applies to L16 vs L16E and PCMU vs PCMU-WB.
	name := parts[1]
	if i := strings.IndexByte(name, '/'); i >= 0 {
		name = name[:i]
	}

	switch name {
	case "opus":
		return uint8(pt64), domain.CodecOpus, true
	case "l16", "linear16":
		return uint8(pt64), domain.CodecL16, true
	case "g722":
		return uint8(pt64), domain.CodecG722, true
	case "pcmu", "ulaw":
		return uint8(pt64), domain.CodecPCMU, true
	case "pcma", "alaw":
		return uint8(pt64), domain.CodecPCMA, true
	default:
		return uint8(pt64), "", false
	}
}
