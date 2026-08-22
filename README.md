# Sendspin VoIP Bridge 📞📢🎶

**Sendspin VoIP Bridge** is designed primarily to deliver **Home Assistant announcements, TTS alerts, doorbell chimes, and intercom paging** directly to your **SIP/VoIP phones and endpoints** (Grandstream, Fanvil, Yealink, Snom, Cisco, Asterisk, FreePBX) with **zero initial speech clipping**, while also supporting synchronized background music.

By bridging **Music Assistant** to standard VoIP hardware, it turns any desk phone, wall intercom, or paging horn into an intelligent, auto-answering Home Assistant smart speaker.

---

## 🌟 What it does

- 📢 **Home Assistant Announcements & TTS (Primary Focus)**: Broadcast doorbell chimes, security alerts, and voice notifications directly to desk phones and paging horns without cutting off the beginning of the speech.
- 🎵 **Streams Background Music**: Play synchronized music to one or more SIP phones when announcements are idle.
- 🚨 **Smart Priority Preemption**: Music automatically pauses or hangs up when a high-priority Home Assistant alert arrives.
- ⚡ **Zero-Config Discovery**: Music Assistant discovers your phones automatically over the local network.
- 🔀 **Multiple Profiles per Phone**: Configure the same physical phone as separate players in Music Assistant (e.g. one for high-volume auto-answering announcements, one for background music, one that rings the handset).

---

## 🚀 Quick Setup

### Option 1: Home Assistant Add-on (Recommended)

[![Open your Home Assistant instance and show the add add-on repository dialog with a specific repository URL pre-filled.](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2Fmiguelangel-nubla%2Fsendspin-voip)

#### Manual Installation

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

## ⚙️ Configuration Reference

### Minimal Working Example (`config.yaml`)

```yaml
sip:
  server: "192.168.1.50:5060"
  username: "sendspin"
  password: "your_sip_password"
  auto_answer_preset: intercom

players:
  - id: "office_phone"
    name: "Office Desk Phone"
    sip_target: "101"
    priority: 10
```

### Full Configuration Options

#### Root Settings
| Option | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `log_level` | string | `info` | Logging verbosity: `debug`, `info`, `warn`, `error`. |
| `state_file` | string | `""` (auto) | File path where volume/mute state is persisted. Defaults to `/data/sendspin-voip-state.json` on Home Assistant, `./sendspin-voip-state.json` elsewhere. |

#### `http` (Web UI & Debug API)
| Option | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `http.listen` | string | `":8080"` | Listening address and port for Web UI dashboard and REST API. |
| `http.api_token` | string | `""` | Optional auth token required via `Authorization: Bearer <token>`, `X-Api-Token`, or `?token=`. |
| `http.enable_pprof` | boolean | `false` | Expose `/debug/pprof` profiling endpoints. |

#### `sip` (SIP PBX & Signaling)
| Option | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `sip.server` | string | *required in PBX mode* | Hostname/IP and port of the SIP PBX or destination (e.g. `"192.168.1.50:5060"`). |
| `sip.username` | string | `"sendspin"` | SIP extension username / auth ID. |
| `sip.password` | string | `""` | SIP account password for PBX authentication. |
| `sip.mode` | string | `"pbx"` | `"pbx"` (registered with credentials) or `"direct"` (peer-to-peer LAN dialing). |
| `sip.domain` | string | `""` (auto) | SIP realm / domain for `From:`/`To:` headers (defaults to `sip.server`). |
| `sip.transport` | string | `"udp"` | Network transport: `"udp"` or `"tcp"`. |
| `sip.local_ip` | string | `""` (auto) | Host LAN IP advertised in SDP. Auto-detected via route to `sip.server` if blank. |
| `sip.local_sip_port` | integer | `5060` | Local UDP/TCP port for SIP listening. |
| `sip.rtp_port_min` | integer | `10000` | Minimum UDP port for dynamic RTP media allocation. |
| `sip.rtp_port_max` | integer | `20000` | Maximum UDP port for dynamic RTP media allocation. |
| `sip.auto_answer_preset` | string | `"default"` | Default auto-answer header preset: `default`, `intercom`, `yealink`, `grandstream`, `snom`, `call_info`, `p_auto`, `none`, `custom`. |
| `sip.custom_auto_answer_header` | string | `""` | Raw SIP header line when `auto_answer_preset` is set to `"custom"`. |

#### `sendspin` (Music Assistant Upstream)
| Option | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `sendspin.server` | string | `"auto"` | `"auto"` discovers Music Assistant via mDNS (`_sendspin._tcp`), or specify `"192.168.1.10:8927"`. |
| `sendspin.buffer_ms` | integer | `500` | Stream pre-buffering window in milliseconds. |

#### `bridge` (Runtime Tuning)
| Option | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `bridge.drain_delay_ms` | integer | `500` | Trailing silence delay in ms before sending SIP BYE (prevents clipping the end of TTS). |
| `bridge.idle_hangup_delay_ms` | integer | `5000` | Linger delay in ms before hanging up an idle SIP call (enables seamless seeks, pauses, and track changes). |
| `bridge.target_conflict_policy` | string | `"preempt_higher"` | Concurrency policy when multiple players target the same phone: `preempt_higher`, `preempt_always`, or `busy`. |

#### `players` (Virtual Player Mappings)
| Option | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `id` | string | *required* | Unique player identifier published to Music Assistant (e.g. `"office_phone"`). |
| `name` | string | same as `id` | Friendly player name shown in Music Assistant and Home Assistant. |
| `sip_target` | string | *required* | Target extension (e.g. `"101"`, `"sip:101"`, or full `"sip:101@192.168.1.50"`). |
| `codec` | string | `"auto"` | Codec to offer: `"auto"` (negotiates highest fidelity: `opus` > `l16` > `g722` > `pcmu` > `pcma`), or lock to specific codec. |
| `auto_answer` | string | inherits from `sip` | Per-player auto-answer preset override (`intercom`, `none`, etc.). |
| `custom_auto_answer_header` | string | `""` | Per-player custom header when `auto_answer: "custom"`. |
| `priority` | integer | `0` | Player priority level (higher numbers preempt lower ones on the same physical phone). |
| `default_volume` | integer | `100` | Initial volume percentage (`0`–`100`). |


---

## 🌐 Network Architecture & Placement (Technical Notes)

Understanding where to place `sendspin-voip` on your network ensures optimal reliability and audio fidelity:

### 1. Colocate Close to SIP Endpoints / PBX (Downstream Leg)
- **Local Network Recommended**: The downstream leg between `sendspin-voip` and your VoIP phones / PBX uses **SIP signaling (UDP 5060)** and **real-time RTP packet transmission (UDP)** at precise 20ms intervals.
- Standard VoIP phones do not maintain deep jitter buffers for live calls.
- To prevent packet loss, jitter, and call signaling delays during auto-answer establishment, **`sendspin-voip` works best placed close network-wise to your SIP phones and PBX** (e.g., on the same LAN / VLAN / site).

### 2. High Resilience Across Remote / WAN Links to Music Assistant (Upstream Leg)
- **Safe Across WAN / Unstable Links**: The upstream connection between **Music Assistant** and **`sendspin-voip`** communicates via TCP / WebSocket streaming.
- Because `sendspin-voip` utilizes **pre-buffering**, audio sent from Home Assistant / Music Assistant is buffered locally before the phone even picks up.
- Consequently, **the link from Music Assistant to `sendspin-voip` can run over high-latency, imperfect, remote, or routed network paths without issue**, as network fluctuations upstream will not cause audio stuttering or truncated syllables during local SIP playback.

---

## 🔄 Dynamic Codec Discovery & Audio Pipelines

`sendspin-voip` features an intelligent, dynamic codec negotiation architecture:

1. **Downstream Capability Probing**: When the bridge starts, it queries the downstream target phone/PBX using SIP `OPTIONS` to discover its exact list of supported audio codecs.
2. **Adaptive Upstream Publishing**: The virtual Sendspin player connects to Music Assistant and advertises format capabilities tailored to that phone:
   - **Opus Phones**: Advertises `Opus 48000Hz 2ch` for **direct end-to-end zero-transcoding passthrough**.
   - **G.722 HD Voice Phones**: Advertises `PCM 16000Hz 1ch` for native 16 kHz wideband speech.
   - **G.711 Narrowband Phones**: Advertises `PCM 8000Hz 1ch` for native 8 kHz narrowband audio.
3. **Dynamic Reconfiguration**: If a phone is reconfigured with new codecs or reconnects, the bridge detects the change every 30s and automatically updates its capability offer on Music Assistant without restarting.
4. **`codec: auto` Mode**: Set `codec: auto` (or omit `codec:`) in your `config.yaml` to automatically use the highest-fidelity codec supported by each phone.

---

## 📊 Web UI & Debug HTTP API

`sendspin-voip` includes an integrated web dashboard and debug API similar to **go2rtc**:

- **Web Dashboard**: Open `http://localhost:8080/` in your browser to inspect active streams, upstream Sendspin ingress audio formats, live track metadata, downstream SIP targets, offered/negotiated codecs, RTP sockets, and packet counters.
- **`GET /api/streams`**: Returns a JSON representation of all registered virtual players, upstream audio sources, codecs, downstream SIP/RTP sessions, and live `audio_path` (passthrough vs transcode stages, volume dB, packet counters).
- **`GET /api/info`**: System metrics, memory usage, goroutines, uptime, and SIP registration status.
- **`GET /api/codecs`**: Specifications of supported audio codecs (Opus, G.722, PCMU, PCMA, PCM).

Security notes for host-network installs:

- Set `http.api_token` (or `HTTP_API_TOKEN`) to require `Authorization: Bearer <token>`, `X-Api-Token`, or `?token=` on all HTTP routes.
- `/debug/pprof` is **disabled by default**; only enable with `http.enable_pprof: true` when debugging locally.

---

## 🛠️ Supported Hardware & Codecs

- **Hardware**: Any generic SIP phone or paging amplifier (Grandstream, Yealink, Fanvil, Snom, Cisco, Polycom, CyberData, Algo, 2N, etc.) or PBX (Asterisk, FreePBX, 3CX, FreeSWITCH).
- **Audio Codecs**: Opus (48kHz passthrough at full volume; CGO-free re-encode with volume/mute via gopus), G.722 (HD Voice / 16kHz wideband), G.711 µ-law / A-law (8kHz narrowband).

---

## 📄 License

MIT License. See [LICENSE](LICENSE) for details.
