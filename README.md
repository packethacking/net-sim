# net-sim — software AX.25 packet network simulator

A software-only simulator for testing AX.25/IL2P link-layer behaviour,
BPQ routing decisions, collision recovery and hidden-node interactions
without real radios. Multiple
[samoyed](https://github.com/doismellburning/samoyed) instances act as
TNC+radio combinations; an audio router (`sim-router`) implements per-link
topology and FM capture-effect mixing between them.

The primary target is **2m FM AX.25** behaviour. SSB and other modulation
schemes are not modelled in v1. Real-radio quirks (TX rise time, RX
recovery, mic AGC, pre/de-emphasis) are explicitly out of scope.

## Architecture

```
                    KISS  TCP                    KISS  TCP
 ┌────────────┐   <─────────>            <─────────>   ┌────────────┐
 │   your     │                                        │  another   │
 │ AX.25 app  │       ┌───────────────────────────┐    │ AX.25 app  │
 │ (BPQ /     │       │       sim-router          │    │ (kissattach│
 │  kissutil) │       │ • parses YAML topology    │    │  / nc /    │
 │            │       │ • spawns N samoyed kids   │    │  ax25d /   │
 │            │       │ • routes audio per link   │    │  ...)      │
 │            │       │ • FM capture mixer per RX │    │            │
 └────────────┘       └─────────────┬─────────────┘    └────────────┘
       ▲                            │                          ▲
       │            stdin (RX PCM)  │  UDP (TX PCM)            │
       │                            ▼                          │
       │              ┌─────────────────────────┐              │
       └─KISS  TCP────┤  samoyed-direwolf #N    ├──KISS  TCP───┘
                      │  (one per simulated     │
                      │   port; modem mode set  │
                      │   by per-port config)   │
                      └─────────────────────────┘
```

The router is **DSP-unaware**: each samoyed child does all modulation /
demodulation; the router moves opaque PCM bytes between them and applies
attenuation, the FM-capture mixing decision, and optional white noise.

The audio path is entirely userspace (`stdin` for RX, UDP datagrams for TX
between samoyed and the router). No PulseAudio, PipeWire, JACK, or
`snd-aloop` is involved. See `NOTES-audio-io.md` for the gory detail.

## Quick start

Prerequisites (Ubuntu 24.04+ / 26.04 LXC tested):

- Go 1.22+ (`apt install golang`)
- `gcc`, `make`, `pkg-config`
- `libudev-dev libhamlib-dev portaudio19-dev libavahi-client-dev libbsd-dev libgps-dev`
  (samoyed build-time dependencies — required even though we won't use any
  audio backend at runtime)
- samoyed checked out and built at `/opt/samoyed`:
  ```
  git clone https://github.com/doismellburning/samoyed /opt/samoyed
  cd /opt/samoyed && make cmds
  ```
- Stock Dire Wolf (`apt install direwolf`) — used as a reference / sanity
  check, not at runtime.

Build the router and the LD_PRELOAD shim that papers over PortAudio's
mandatory init (also explained in `NOTES-audio-io.md`):

```
make build
```

Run the smallest demo:

```
make demo-two-node
```

This brings up two AFSK1200 stations, fully linked, KISS exposed at
`127.0.0.1:8001` and `127.0.0.1:8002`. Connect from another shell:

```
# Send a manually crafted KISS frame to A
nc 127.0.0.1 8001 < some_kiss_frame.bin

# Watch decoded frames come out of B
nc 127.0.0.1 8002 | xxd
```

Or attach BPQ / kissutil / kissattach directly to those ports — the router
doesn't touch KISS frames, samoyed handles them natively.

## Demo topologies

| Target | What it shows |
|---|---|
| `make demo-two-node` | Single bidirectional AFSK1200 link. Sanity check. |
| `make demo-two-node-noisy` | Same plus deliberate path loss and noise — Phase 4: FER climbs visibly. |
| `make demo-hidden-node` | A↔B, B↔C, no A↔C; equal loss. Simultaneous TX from A and C produces collision at B. |
| `make demo-hidden-node-capture` | Same topology, A loud / C quiet. A captures the demodulator at B; C is suppressed. |
| `make demo-mesh-3` | Three-node fully connected mesh. |
| `make demo-linear-6` | A — B — C — D — E — F chain, each hears only immediate neighbours. |
| `make demo-star-6` | Hub + 5 spokes. |
| `make demo-multiport-3` | Three nodes; the middle one has two independent radio ports. |

Each demo prints its KISS port assignments at startup.

## YAML config

```yaml
mixer_mode: fm_capture        # fm_capture (default) | linear_sum (stub)
capture_db: 6.0               # FM capture ratio
collision_mode: silence       # silence (default) | sum (stub) | noise (stub)

nodes:
  - id: a
    callsign: SIMA            # used for samoyed's MYCALL
    ports:
      - id: vhf               # unique within the node
        modem: { mode: afsk1200 }
        kiss_port: 8001       # the host-side TCP port for this port's KISS
      - id: uhf-link
        modem: { mode: gfsk9600 }
        kiss_port: 8002
  - id: b
    callsign: SIMB
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8003

links:
  # Directional. Both endpoints must use compatible modem configs.
  - { from: a.vhf, to: b.vhf, loss_db: 0 }
  - { from: b.vhf, to: a.vhf, loss_db: 0 }
```

Strict parsing: any unknown key inside a `modem:` block (e.g. `baud_rate`
when you meant `baud`) is an error at startup, not a silent default.

### Modem catalogue

| `mode` | Required params | Status (current samoyed) |
|---|---|---|
| `afsk1200` | none | ✅ supported. Default workhorse. |
| `gfsk9600` | none | ✅ supported. Auto-selected for 9600 baud + G3RUH. |
| `bpsk` | `baud`, optional `carrier_hz` | ❌ not yet — refused at startup. |
| `il2p` | `inner` (afsk1200 / gfsk9600), `crc` (bool) | ✅ supported. |

Modes the YAML accepts but samoyed can't actually run *fail at startup
with a clear error*. Adding a new mode is config plumbing — see the
translation table in `internal/samoyed/child.go` and the source-of-truth
table in `NOTES-audio-io.md`.

## Why FM capture-effect mixing (and not linear sum)

A linear-sum mixer is simpler and is what most "audio bus" libraries
default to. It is also wrong for the dominant use case of this rig.

Real FM receivers exhibit the **capture effect**: when two FM signals
arrive simultaneously, the stronger one takes over the demodulator and
the weaker is suppressed, *provided* the level difference exceeds a
threshold (the "capture ratio", typically ~6 dB on narrow-band 2m FM).
This is why hidden-node collisions on 2 m packet are *intermittent*
rather than universal: a stronger station regularly punches through.

`sim-router` implements receiver-side capture-effect mixing per port:

```
for each rx block per receiving port:
  active := list of TX streams reaching this port right now
  if len(active) == 0: silence
  if len(active) == 1: that stream, attenuated per loss_db
  if len(active) >= 2:
    sort by RX level; margin = strongest − next
    if margin >= capture_db:  output strongest, attenuated  (capture)
    else:                     collision garbage              (silence in v1)
```

This is what makes the `hidden-node` and `hidden-node-capture` demos do
qualitatively different things from the same code.

## What's not in v1

- BPQ / XRouter integration (works fine — point them at the KISS ports —
  but no demos shipped).
- Hot reload of topology (restart the router).
- Web UI / topology visualisation. Logs to stderr, that's the interface.
- Recording / playback of test runs.
- BER / FER reporting beyond the basic frame counters demonstrable from
  KISS sniffing.
- SSB modelling, AGC, pre/de-emphasis, multipath, Doppler, fading.
- `linear_sum` / `sum` / `noise` mixer modes — accepted in the YAML, only
  `fm_capture` + `silence` are functional in v1.
- Modem modes beyond what samoyed currently supports.

## Layout

```
cmd/sim-router/main.go     - entrypoint
internal/config/           - YAML parsing + validation (strict)
internal/samoyed/          - per-port samoyed-direwolf process management
                             + the modem-mode → CLI-flags translation
internal/audio/            - PCM types, FM-capture mixer, noise generator
internal/router/           - topology, audio routing, glue
configs/                   - demo topology YAMLs
preload/pa_stub.c          - 6-line LD_PRELOAD shim for libportaudio
NOTES-audio-io.md          - Phase 1 findings (essential reading if you
                             want to understand why we use stdin + UDP)
```
