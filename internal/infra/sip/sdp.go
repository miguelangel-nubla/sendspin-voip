package sip

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// BuildSDPOffer creates an SDP offer string for the specified codec and local RTP port.
func BuildSDPOffer(localIP string, rtpPort int, codec domain.Codec) (string, error) {
	if localIP == "" {
		localIP = "127.0.0.1"
	}

	sd := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      123456,
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

	// Build ordered codec list with preferred codec first, followed by fallbacks in fidelity order
	allCodecs := []domain.Codec{codec}
	fallbackCodecs := []domain.Codec{domain.CodecOpus, domain.CodecL16, domain.CodecG722, domain.CodecPCMU, domain.CodecPCMA}
	for _, c := range fallbackCodecs {
		if c != codec {
			allCodecs = append(allCodecs, c)
		}
	}

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
			rtpmapAttrs = append(rtpmapAttrs, sdp.NewAttribute("rtpmap", "97 L16/48000/2"))
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

			// Check negotiated format
			for _, fmtStr := range m.MediaName.Formats {
				pt, _ := strconv.Atoi(fmtStr)
				switch uint8(pt) {
				case 9:
					selectedCodec = domain.CodecG722
				case 8:
					selectedCodec = domain.CodecPCMA
				case 0:
					selectedCodec = domain.CodecPCMU
				case 96:
					selectedCodec = domain.CodecOpus
				case 10, 11, 97:
					selectedCodec = domain.CodecL16
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
func ParseSDPCodecs(sdpRaw string) []domain.Codec {
	sd := &sdp.SessionDescription{}
	if err := sd.Unmarshal([]byte(sdpRaw)); err != nil {
		return nil
	}

	var codecs []domain.Codec
	seen := make(map[domain.Codec]bool)

	for _, m := range sd.MediaDescriptions {
		if strings.EqualFold(m.MediaName.Media, "audio") {
			for _, attr := range m.Attributes {
				if strings.EqualFold(attr.Key, "rtpmap") {
					val := strings.ToLower(attr.Value)
					if strings.Contains(val, "opus") && !seen[domain.CodecOpus] {
						codecs = append(codecs, domain.CodecOpus)
						seen[domain.CodecOpus] = true
					} else if (strings.Contains(val, "l16") || strings.Contains(val, "linear16")) && !seen[domain.CodecL16] {
						codecs = append(codecs, domain.CodecL16)
						seen[domain.CodecL16] = true
					} else if strings.Contains(val, "g722") && !seen[domain.CodecG722] {
						codecs = append(codecs, domain.CodecG722)
						seen[domain.CodecG722] = true
					} else if (strings.Contains(val, "pcmu") || strings.Contains(val, "ulaw")) && !seen[domain.CodecPCMU] {
						codecs = append(codecs, domain.CodecPCMU)
						seen[domain.CodecPCMU] = true
					} else if (strings.Contains(val, "pcma") || strings.Contains(val, "alaw")) && !seen[domain.CodecPCMA] {
						codecs = append(codecs, domain.CodecPCMA)
						seen[domain.CodecPCMA] = true
					}
				}
			}

			for _, fmtStr := range m.MediaName.Formats {
				pt, _ := strconv.Atoi(fmtStr)
				var c domain.Codec
				switch uint8(pt) {
				case 9:
					c = domain.CodecG722
				case 0:
					c = domain.CodecPCMU
				case 8:
					c = domain.CodecPCMA
				case 96, 97, 98, 111:
					c = domain.CodecOpus
				}
				if c != "" && !seen[c] {
					codecs = append(codecs, c)
					seen[c] = true
				}
			}
		}
	}
	return codecs
}
