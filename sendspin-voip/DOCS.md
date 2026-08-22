# Sendspin VoIP Bridge for Home Assistant

Bridge **Music Assistant** to generic SIP/VoIP phones (Grandstream, Fanvil, Yealink, Snom, Cisco, Asterisk/FreePBX) primarily for **Home Assistant TTS announcements, doorbell chimes, and intercom paging**, with support for synchronized background music.

## Features

- 📢 **Home Assistant Announcements & Alerts**: Reliable auto-answering playback for TTS, doorbells, and notifications with zero initial speech clipping.
- ⚡ **Native Sendspin Protocol**: Zero-configuration discovery in Music Assistant via mDNS and WebSockets.
- 🎵 **Pure Go SIP & RTP Stack**: Built-in support for G.722 (HD Voice), PCMU (µ-law), PCMA (A-law), and Opus.
- 🤖 **Smart Auto-Answer**: Pre-configured auto-answer headers for Grandstream, Yealink, Snom, Fanvil, and custom headers.
- ⏱️ **Zero-Speech-Clipping Pre-Buffering**: Automatically accumulates audio during SIP call setup to prevent clipping initial syllables when the phone answers.
- 🔀 **Multi-Player Virtualization**: Configure the same physical desk phone as multiple players (e.g. one for high-volume paging announcements, one for background music).
- 🚨 **Target Concurrency Arbiter**: Automatically preempts background music for high-priority emergency announcements.

## 🌐 Network Placement (Technical Notes)

- **SIP & RTP Downstream**: `sendspin-voip` should be placed close network-wise to your SIP phones/PBX (same local LAN or low-jitter network), as downstream RTP audio uses real-time UDP.
- **Music Assistant Upstream**: The upstream connection to Music Assistant runs over TCP/WebSockets and leverages pre-buffering. This means Music Assistant can reside across WAN links, remote networks, or high-latency segments without causing audio stutter or lost syllables.

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
    priority: 10

  - id: "office_phone_music"
    name: "Office Desk (Music)"
    sip_target: "sip:101@192.168.1.50"
    codec: "g722"
    priority: 1
```


