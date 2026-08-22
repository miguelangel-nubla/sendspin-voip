# Development Guide

This document contains technical details for developers contributing to `sendspin-voip`.

---

## 🏛️ Architecture

`sendspin-voip` is built following Clean Architecture:

- **`internal/domain`**: Business rules, player models, session states, and target conflict & preemption arbiter. No external dependencies.
- **`internal/app`**: Application use-cases and ports (`BridgeService`, `PlayerIngressPort`, `SIPCallerPort`, `RTPStreamerPort`).
- **`internal/infra`**: Concrete adapters:
  - `config`: YAML, JSON, and Home Assistant `/data/options.json` loader.
  - `sendspin`: Sendspin wire protocol client with mDNS discovery.
  - `sip`: SIP user agent using `sipgo` and SDP negotiation.
  - `rtp`: RTP packetizer and 20ms pacer using Pion RTP.
  - `audio`: Stereo-to-mono downmixer, linear resampler, and pure-Go G.711 / G.722 codecs.
- **`cmd/sendspin-voip`**: Dependency injection wiring and graceful shutdown handling.

---

## 🌐 Network Placement & Prebuffering Considerations

- **Primary Goal**: The application is primarily designed to deliver Home Assistant announcements (TTS, doorbell rings, alerts) to SIP endpoints with zero initial syllable clipping.
- **Downstream Placement**: `sendspin-voip` should be colocated network-wise close to the SIP endpoints / PBX. Downstream RTP uses unbuffered, real-time UDP pacing (20ms frames). Proximity ensures minimal jitter, low latency, and prevents packet loss on hardware phones with shallow jitter buffers.
- **Upstream Resilience**: The upstream transport from Music Assistant uses TCP/WebSocket streaming. With pre-buffering, audio is accumulated while the SIP call is establishing. Therefore, the connection between Music Assistant and `sendspin-voip` can span high-latency, imperfect WAN or cross-VLAN paths without causing audio artifacts.


---

## 🛠️ Building & Testing

```bash
# Setup git pre-commit hook
make hooks

# Run code formatters and static checks
make fmt
make vet

# Run all unit tests
make test

# Run race detector tests
make test-race

# Run all verification checks (formatting, vet, test, race, cross-compilation)
make check

# Compile static binary
make build

# Build local Docker image
make docker
```


