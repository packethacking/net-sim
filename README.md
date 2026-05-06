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

## Quick run (Docker)

Pre-built images on ghcr:

```
docker pull ghcr.io/packethacking/net-sim:main
```

Bundled default network (two AFSK1200 nodes, KISS on `8001`/`8002`):

```
docker run --rm -p 8080:8080 -p 8001:8001 -p 8002:8002 \
  ghcr.io/packethacking/net-sim:main
```

Open <http://localhost:8080> for the web UI; KISS-attach your AX.25
application to `localhost:8001` / `localhost:8002`.

For a custom topology — point at any YAML on the host, and use
`--network=host` so KISS ports can land anywhere without you having to
predict them:

```
docker run --rm --network=host \
  -v $PWD/my-network.yaml:/etc/sim/network.yaml \
  ghcr.io/packethacking/net-sim:main
```

Tags published:
- `:main` and `:main-<sha>` on every push to main
- `:vX.Y.Z`, `:X.Y`, `:X`, `:latest` on tagged releases

Embedding in another project's tests (e.g. as a fixture for your AX.25
client / BPQ-style router): mount your network YAML, expose the KISS
ports your test connects to, and use the two probe endpoints —

- `GET /healthz` → `200 ok` once the HTTP server is accepting connections
  (use as a readiness probe).
- `GET /api/status` → JSON; check `running:true` to confirm the router
  brought all the samoyed children up.

The default Docker `CMD` is `-autostart`, so `running:true` is the
expected steady state right after startup. Override (`docker run ...
ghcr.io/.../net-sim:main` with extra args) if you'd rather drive
Start/Stop manually from your test harness via `POST /api/start`.

## Quick install (curl | sudo bash)

On a fresh Debian 12 / Ubuntu 24.04+ host (LXC, VM, bare metal — anywhere
you have root and apt):

```
curl -fsSL https://raw.githubusercontent.com/packethacking/net-sim/main/install.sh | sudo bash
```

That script installs apt build-deps, clones and builds samoyed at
`/opt/samoyed`, clones and builds net-sim at `/opt/sim`, drops the
LD_PRELOAD shim into `/usr/local/lib/libpa_stub.so`, installs the
binaries to `/usr/local/bin/`, bootstraps a default two-node network at
`/etc/sim/network.yaml`, and (where systemd is present) registers and
starts a `sim-web.service` listening on `:8080`.

Then open <http://your-host:8080/>. The default page lets you edit the
YAML topology and Start / Stop / Apply-and-restart the simulator.

Override knobs (set as env vars before `sudo bash`):

| Var | Default | Notes |
|---|---|---|
| `SIM_DIR` | `/opt/sim` | net-sim checkout |
| `SAMOYED_DIR` | `/opt/samoyed` | samoyed checkout |
| `NETWORK_YAML` | `/etc/sim/network.yaml` | the active config |
| `WEB_PORT` | `8080` | sim-web listen port |
| `SYSTEMD` | `1` | set to `0` to skip the unit |
| `SIM_REF` / `SAMOYED_REF` | `main` | git ref to check out |

The script is idempotent — re-run it to update to a newer `main`. It
does **not** install pulseaudio / pipewire / jackd; the LD_PRELOAD shim
keeps PortAudio happy without a sound stack (see `NOTES-audio-io.md`).

## Web UI

`sim-web` is a small integrated control surface. One page, one config
file, three buttons:

- **Start** — load the YAML and bring up the router with one samoyed
  child per port.
- **Apply & restart** — save the textarea contents to the YAML file
  (validated strictly) and recycle the router.
- **Stop** — tear it all down.

KISS TCP ports are listed live as the topology comes up so you know
where to point your AX.25 application.

`sim-web` embeds the router; it doesn't shell out to a separate
`sim-router` binary. Either one is a fine entrypoint:

- Engineer-loop / scripting: `sim-router -config configs/two-node.yaml`
- Day-to-day editing: the web UI.

## Manual install / build from source

If you'd rather not run an installer, the equivalent steps:

Prerequisites (Debian/Ubuntu):

- Go 1.22+ (`apt install golang`)
- `gcc`, `make`, `pkg-config`
- `libudev-dev libhamlib-dev portaudio19-dev libavahi-client-dev libbsd-dev libgps-dev libasound2-dev`
  (samoyed build-time deps — required even though we won't use any audio
  backend at runtime)
- samoyed checked out and built at `/opt/samoyed`:
  ```
  git clone https://github.com/doismellburning/samoyed /opt/samoyed
  make -C /opt/samoyed cmds
  ```
- Stock Dire Wolf (`apt install direwolf`) — reference only, not used at
  runtime.

Build:

```
make build       # builds sim-router, sim-web, and /usr/local/lib/libpa_stub.so
```

Run the smallest demo:

```
make demo-two-node
```

Two AFSK1200 stations, fully linked, KISS exposed at `127.0.0.1:8001`
and `127.0.0.1:8002`. From another shell:

```
nc 127.0.0.1 8001 < some_kiss_frame.bin
nc 127.0.0.1 8002 | xxd                # see decoded frames
```

Or attach BPQ / kissutil / kissattach directly to those ports — the
router doesn't touch KISS frames, samoyed handles them natively.

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
    ports:
      - id: vhf               # unique within the node
        modem: { mode: afsk1200 }
        kiss_port: 8001       # the host-side TCP port for this port's KISS
      - id: uhf-link
        modem: { mode: gfsk9600 }
        kiss_port: 8002
  - id: b
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
| `il2p` | `inner` (afsk1200 / gfsk9600), `fec` (`strong`/`weak`) | ✅ supported (FEC strength only — see Known limitations). |

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

## Known limitations (samoyed-side, expected to be fixed upstream)

These are gaps in the current samoyed build that affect what you can
test against; both will likely land in samoyed soon and we'll bump the
pin then. Tracker issues:
[net-sim#1](https://github.com/packethacking/net-sim/issues/1) /
[net-sim#2](https://github.com/packethacking/net-sim/issues/2).

- **No IL2P+CRC (a.k.a. IL2Pc) support.** Samoyed implements the IL2P
  v0.6 base form — header + payload, Reed-Solomon FEC, no trailing 2-byte
  CRC. The `fec` field on `il2p` modems toggles RS strength (`strong` →
  `-I 1`, `weak` → `-I 0`); it does *not* enable the spec's optional
  trailing-CRC variant. If your application requires IL2Pc on the wire,
  this rig won't reproduce it yet. (The field used to be called `crc`,
  which was misleading — renamed to `fec` to match what it actually
  does.)
- **No KISS ACKMODE.** Samoyed's KISS layer explicitly refuses XKISS
  opcodes (`12 = ACKMODE data`, `14 = poll`) — sending one logs
  `Using ACKMODE will cause this error.` and the frame is dropped.
  Anything that depends on tracked-frame ACKs from the TNC (some BPQ
  configurations, certain `ax25d` setups) won't work against simulated
  ports. Use NORMAL KISS only.

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
cmd/sim-router/            - CLI entrypoint
cmd/sim-web/               - integrated web UI; embeds the router
internal/config/           - YAML parsing + validation (strict)
internal/samoyed/          - per-port samoyed-direwolf process management
                             + the modem-mode → CLI-flags translation
internal/audio/            - PCM types, FM-capture mixer, noise generator
internal/router/           - topology, audio routing, glue
configs/                   - demo topology YAMLs
preload/pa_stub.c          - 6-line LD_PRELOAD shim for libportaudio
install.sh                 - curl | sudo bash installer
NOTES-audio-io.md          - Phase 1 findings (essential reading if you
                             want to understand why we use stdin + UDP)
```
