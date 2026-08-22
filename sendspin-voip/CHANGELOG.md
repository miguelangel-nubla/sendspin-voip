# Changelog

## 1.0.1

- Explicit network destination routing for PBX servers when SIP server hostname/IP differs from the realm domain.
- Support shorthand SIP target extension numbers (e.g. `8003` or `sip:8003`) with automatic PBX domain qualification.
- Simplified `config.example.yaml` with commented-out optional settings and complete Configuration Reference table in docs.

## 1.0.0

- Initial release of Sendspin VoIP Bridge for Home Assistant.
- Native Sendspin WebSocket protocol & mDNS discovery support for Music Assistant.
- Pure Go SIP / RTP stack (G.722, G.711u, G.711a, Opus).
- Smart zero-clipping pre-buffering and seamless stream transitions.
- Multi-player mapping for single physical SIP endpoints.
- High-priority preemption arbiter for emergency and paging announcements.
