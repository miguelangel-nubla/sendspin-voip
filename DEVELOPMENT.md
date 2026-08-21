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

## 🛠️ Building & Testing

```bash
# Run all unit tests
make test

# Compile static binary
make build

# Build local Docker image
make docker
```
