# Sendspin VoIP Bridge 📞🎶

**Sendspin VoIP Bridge** connects **Music Assistant** to your **SIP/VoIP phones and intercoms** (Grandstream, Fanvil, Yealink, Snom, Cisco, Asterisk, FreePBX), turning any standard VoIP phone into a multi-room smart speaker or whole-house paging endpoint.

---

## 🌟 What it does

- 📢 **Plays TTS & Announcements**: Send doorbell chimes, Home Assistant alerts, and voice notifications directly to desk phones or paging horns without cutting off the beginning of the speech.
- 🎵 **Streams Music**: Play synchronized music to one or more SIP phones.
- ⚡ **Zero-Config Discovery**: Music Assistant discovers your phones automatically over the local network.
- 🔀 **Multiple Profiles per Phone**: Configure the same physical phone as separate players in Music Assistant (e.g. one for loud auto-answering announcements, one for background music, one that rings the handset).
- 🚨 **Smart Interruptions**: Music automatically pauses or hangs up when a high-priority announcement arrives.

---

## 🚀 Quick Setup

### Option 1: Home Assistant Add-on (Recommended)

1. In Home Assistant, go to **Settings** → **Add-ons** → **Add-on Store**.
2. Click the three dots in the top-right corner (⋮) → **Repositories**.
3. Add the repository URL:
   ```
   https://github.com/miguelangel-nubla/sendspin-voip
   ```
4. Find **Sendspin VoIP Bridge** in the store, click **Install**, and configure your SIP details in the **Configuration** tab.
5. Click **Start**. Your configured phones will instantly appear in Music Assistant!

---

### Option 2: Docker Compose

```yaml
version: '3.8'

services:
  sendspin-voip:
    image: ghcr.io/miguelangel-nubla/sendspin-voip:latest
    container_name: sendspin-voip
    restart: unless-stopped
    network_mode: host
    volumes:
      - ./config.yaml:/app/config.yaml:ro
    environment:
      - LOG_LEVEL=info
```

> **Note**: `network_mode: host` is required so the bridge can discover Music Assistant on your network and stream audio over UDP.

---

## ⚙️ Configuration Example (`config.yaml`)

```yaml
# SIP PBX / Server Connection
sip:
  server: "192.168.1.50:5060"
  username: "sendspin"
  password: "your_sip_password"
  domain: "192.168.1.50"
  auto_answer_preset: "default" # default, intercom, yealink, grandstream, snom, none

# Your configured phones / players
players:
  # Example 1: Office Desk Phone for Announcements (Forces auto-answer, zero speech clipping)
  - id: "office_phone_announcement"
    name: "Office Desk (Announcements)"
    sip_target: "sip:101@192.168.1.50"
    codec: "g722" # HD Voice
    buffer_mode: "announcement"
    auto_answer: "intercom"
    priority: 10
    default_volume: 100

  # Example 2: Office Desk Phone for Background Music (Synchronized playback)
  - id: "office_phone_music"
    name: "Office Desk (Music)"
    sip_target: "sip:101@192.168.1.50"
    codec: "g722"
    buffer_mode: "live"
    auto_answer: "intercom"
    priority: 1
    default_volume: 50

  # Example 3: Doorbell / Ring Chime (Rings the phone instead of speakerphone)
  - id: "office_phone_doorbell"
    name: "Office Phone (Doorbell Ring)"
    sip_target: "sip:101@192.168.1.50"
    codec: "pcmu"
    buffer_mode: "announcement"
    auto_answer: "none"
    priority: 5
    default_volume: 80
```

---

## 🎛️ Playback & Buffering Modes

| Mode | Description | Best For |
| :--- | :--- | :--- |
| **`announcement`** (Default) | Holds audio during the SIP call setup and plays from the very beginning once the phone answers (prevents speech clipping). | Doorbell alerts, TTS announcements, intercom paging. |
| **`live`** | Discards pre-answer buffering to immediately lock into real-time audio playback. | Background music and synchronized playback. |

---

## 🛠️ Supported Hardware & Codecs

- **Hardware**: Any generic SIP phone or paging amplifier (Grandstream, Yealink, Fanvil, Snom, Cisco, Polycom, CyberData, Algo, 2N, etc.) or PBX (Asterisk, FreePBX, 3CX, FreeSWITCH).
- **Audio Codecs**: G.722 (HD Voice / 16kHz wideband), G.711 µ-law / A-law (8kHz narrowband), and Opus (48kHz direct stream passthrough and decode).

---

## 📄 License

MIT License. See [LICENSE](LICENSE) for details.
