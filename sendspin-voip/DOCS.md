# Sendspin VoIP Bridge for Home Assistant

Bridge **Music Assistant** to generic SIP/VoIP phones (Grandstream, Fanvil, Yealink, Snom, Cisco, Asterisk/FreePBX) for announcements, intercom paging, and synchronized music playback.

## Features

- **Native Sendspin Protocol**: Zero-configuration discovery in Music Assistant via mDNS and WebSockets.
- **Pure Go SIP & RTP Stack**: Built-in support for G.722 (HD Voice), PCMU (µ-law), PCMA (A-law), and Opus.
- **Smart Auto-Answer**: Pre-configured auto-answer headers for Grandstream, Yealink, Snom, Fanvil, and custom headers.
- **Zero-Speech-Clipping Buffering**: Configure `announcement` mode to hold audio until the phone answers, or `live` mode for real-time background music.
- **Multi-Player Virtualization**: Configure the same physical desk phone as multiple players (e.g. one for high-volume paging announcements, one for background music).
- **Target Concurrency Arbiter**: Automatically preempts background music for high-priority emergency announcements.

## Configuration

In the Add-on Configuration tab, specify your SIP server/PBX credentials and list the players mapping to your SIP extensions.

```yaml
sip:
  server: "192.168.1.50:5060"
  username: "sendspin"
  password: "your_sip_password"
  domain: "192.168.1.50"
  auto_answer_preset: "default"

players:
  - id: "office_phone_announcement"
    name: "Office Desk (Announcements)"
    sip_target: "sip:101@192.168.1.50"
    codec: "g722"
    buffer_mode: "announcement"
    priority: 10

  - id: "office_phone_music"
    name: "Office Desk (Music)"
    sip_target: "sip:101@192.168.1.50"
    codec: "g722"
    buffer_mode: "live"
    priority: 1
```
