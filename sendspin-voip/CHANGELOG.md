# Changelog

## [1.0.3](https://github.com/miguelangel-nubla/sendspin-voip/compare/v1.0.2...v1.0.3) (2026-08-22)


### Bug Fixes

* **ci:** grant packages write permission for release-please reusable workflow ([7f87394](https://github.com/miguelangel-nubla/sendspin-voip/commit/7f87394de89b09d57a862a3ee243d22a4caa8c45))
* **ci:** make tag_name input optional for reusable release workflow ([304adf6](https://github.com/miguelangel-nubla/sendspin-voip/commit/304adf6a062b5e8a16f8382df6fd6ecebfe1d282))

## 1.0.2

- feat: centralize version metadata and add automated release workflow
- docs: remove 'show an add-on' badge from README
- docs: add My Home Assistant 1-click install badges to README

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
