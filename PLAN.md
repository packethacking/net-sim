# AX.25 Network Simulator — Implementation Plan

You're picking up an existing design discussion. The architecture below was
agreed upon already; your job is to implement it. Don't re-litigate the
fundamental design — push back only if you hit a hard blocker, and only after
trying the documented approach.

## What we're building

A software-only AX.25 packet radio network simulator. Multiple instances of
[samoyed](https://github.com/doismellburning/samoyed) (a Go port of Dire Wolf)
play the role of TNC+radio combinations, connected via a software audio
router that implements per-link topology — which simulated nodes can hear
which, with optional path loss and noise per link.

The primary target is **2m FM AX.25** behaviour, including the FM capture
effect (see Phase 3). SSB and other modulation schemes are not modelled in v1.

The simulator supports **multi-port nodes** — a single logical node may have
several independent radio ports (different "frequencies", different modems),
the way real packet nodes have multiple TNCs. Links connect ports, not nodes.

Target deliverable: demonstrate the classic hidden-node problem with three
nodes, then scale to 6+ nodes (some multi-port) with arbitrary topology
defined in a YAML config file.

The point is to test AX.25/IL2P link-layer behaviour, BPQ routing decisions,
collision recovery, hidden-node interactions, etc., without needing real
radios. Real-radio quirks (TX rise time, RX recovery, mic AGC, pre/de-emphasis)
are explicitly out of scope — that's not what this rig is for.

## Architecture

Three pieces:

1. **N × samoyed instances** — one per simulated *port* (a node with three
   ports has three samoyed processes). Each runs a single channel with a
   single modem mode. Audio in/out via TCP raw PCM (likely mono / 48 kHz
   / signed 16-bit LE, confirm in Phase 1). KISS over TCP exposed for
   whatever the user wants to plug in (BPQ, kissattach, manual nc).

2. **Router process (Go)** — `sim-router`. Owns topology and audio routing.
   For each TX from each port, fans the audio to receiving ports per the
   topology matrix, applying:
   - Per-link attenuation (dB)
   - Per-link optional white noise
   - PTT-gated self-mute (a port never hears its own TX)
   - **FM capture-effect mixing** at each receiver (see Phase 3)

   The router is **DSP-unaware**. It treats audio as opaque PCM bytes. The
   modem field in YAML is configuration *passed through* to the spawned
   samoyed child, plus *validated* for cross-link compatibility. The router
   does not modulate, demodulate, or care what mode is in use.

3. **Topology config (YAML)** — defines nodes, their ports (with modem
   config), and per-link parameters. Hot reload not required for v1;
   restart the router to change topology.

Audio routing is all userspace TCP rather than `snd-aloop`/PipeWire because
(a) we may not have kernel-loopback modules available on this LXC kernel,
and (b) keeping audio in userspace makes the whole thing portable, testable
in CI, and trivial to reason about. No `asound.conf`, no PipeWire graph
debugging.

### Terminology

- **Node**: a logical station (one callsign, one BPQ instance worth of
  identity). Has 1+ ports.
- **Port**: a TNC+radio combination — one samoyed process, one modem
  mode, one KISS interface. Same terminology BPQ uses.
- **Link**: a directional audio path from one port to another. Both ends
  must use compatible modem configs. Has a `loss_db` and optional
  `noise_db`.
- **`kiss_port`**: the TCP port number on which this port's KISS interface
  is exposed (terminology collision unavoidable; "port" the model concept,
  "kiss_port" the network port number).

## Modem catalogue

The set of `modem.mode` values the YAML accepts. Whether a given mode
actually *works* depends on what the underlying samoyed (or Dire Wolf
fallback) supports today — the YAML shape is forward-looking; the
mode→samoyed-flags translation table is implementation.

| `mode` | Required params | Notes |
|---|---|---|
| `afsk1200` | none | Classic Bell 202 AFSK, 2m FM. **Default workhorse.** |
| `gfsk9600` | none | G3RUH 9600 GFSK. |
| `bpsk` | `baud`, optional `carrier_hz` | HF BPSK. Awaiting samoyed support. |
| `il2p` | `inner` (one of the above), `crc` (bool) | IL2P-wrapped, with or without the +CRC variant. Awaiting samoyed support. |

Future modes (PSK variants, OFDM, etc.) get added as new rows. **Adding a
new mode is a config plumbing change, not a router rewrite** — the router
just learns to translate it to the appropriate samoyed CLI flags.

For v1, only the modes that current samoyed actually supports need to
work end-to-end. Modes listed above that samoyed doesn't support yet:
the YAML parser must accept and validate them, but the router should
refuse to start with a clear "modem mode X not yet supported by current
samoyed binary" error if a config requests one. Don't silently fail.

The Phase 1 investigation must include enumerating what samoyed currently
supports and recording it in `NOTES-audio-io.md`. The translation table
in code lives next to that document conceptually.

## Phases

Implement in order. Don't skip Phase 1 — Phase 1 findings determine
everything downstream.

### Phase 0 — Environment

On the LXC (Ubuntu, root):

- Install: `golang` (latest stable in apt or via official tarball if too
  old), `build-essential`, `git`, `make`, `libasound2-dev`, `tcpdump`,
  `socat`, `pkg-config`, `yamllint`.
- Clone samoyed into `/opt/samoyed`, build with `make cmds`. Verify the
  binary runs.
- Clone stock Dire Wolf into `/opt/direwolf`, build it. This is reference
  only — used to A/B test samoyed behaviour, not a runtime dependency.

Acceptance: samoyed binary runs and prints sensible help/usage.

### Phase 1 — Audio I/O investigation (most important phase)

Determine how to feed audio into samoyed and read TX audio out, *without*
going through any kernel sound stack. Read the samoyed source under `src/`,
specifically anything related to `ADEVICE` or audio device handling.

Try, in order of preference:

1. **stdin/stdout audio** — does `ADEVICE -` (or equivalent) work for raw
   PCM on stdin? Stock Dire Wolf supports this for a single channel.
2. **TCP audio source/sink** — Dire Wolf has had varying levels of TCP
   audio support over the years. Check what samoyed inherits.
3. **Named pipe** — `mkfifo` and pass it as the audio device path.
4. **stock Dire Wolf as a fallback** — if samoyed lacks a viable
   non-kernel audio path entirely, drop back to stock Dire Wolf for v1
   and document this. Don't try to add audio I/O features to samoyed as
   part of this work — that's a separate contribution to a friend's
   project, file an issue and move on.

Test each candidate by feeding it a known-good WAV (the WA8LMF TNC test
recording is bundled in Dire Wolf's `doc/` tree) and verifying samoyed
decodes the expected frame count. Compare against stock Dire Wolf decoding
the same WAV — they should agree.

**Also enumerate which modem modes the current samoyed supports.** Run
through the modem catalogue above; for each, find the CLI flags or config
needed to put samoyed in that mode. Record results in
`NOTES-audio-io.md`. This becomes the source of truth for the
mode→samoyed-flags translation table in Phase 2.

Document findings in `/opt/sim/NOTES-audio-io.md`. Include the exact
samoyed command line that works, the audio format expected, the modem
modes available, and any quirks discovered.

Acceptance: a documented shell pipeline that gets PCM into and out of
samoyed without ALSA, decoding the WA8LMF test signal correctly. Plus a
table of currently-supported modem modes with their invocation.

### Phase 2 — Router skeleton

Create `/opt/sim` as a new Go module. Single binary: `sim-router`.

Scope:
- Parse YAML config matching the schema in the next subsection
- Validate the config:
  - Every link references real `node.port` pairs
  - Both ends of every link use compatible modem configs (same `mode`
    and same parameters; mismatched modems are rejected at startup with
    a clear error)
  - No requested modem mode is one that samoyed-on-this-host doesn't
    support — also a hard startup error
- For each port across all nodes, spawn a samoyed child process configured
  via the mode→flags translation. Each samoyed gets its own audio I/O
  channels and KISS port.
- Naive audio routing: per-link, copy bytes from source port's TX to
  dest port's RX, no mixing logic yet, no muting yet. Just plumbing.
- Forward each port's KISS interface to the configured host-side TCP port
- Verbose logging of every TX-byte routed, behind a `-v` flag

#### YAML schema

```yaml
# Top-level mixer config
mixer_mode: fm_capture       # fm_capture (default) | linear_sum (stub)
capture_db: 6.0              # FM capture ratio
collision_mode: silence      # silence (default) | sum (stub) | noise (stub)

nodes:
  - id: rdg
    ports:
      - id: vhf              # port id, unique within the node
        modem:
          mode: afsk1200
        kiss_port: 8001      # host-side TCP port for this port's KISS
      - id: uhf-link         # multi-port nodes have more than one entry
        modem:
          mode: gfsk9600
        kiss_port: 8002

  - id: bsg
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8003

links:
  # Links reference node.port pairs and are directional.
  # Both ends must use compatible modem configs.
  - from: rdg.vhf
    to:   bsg.vhf
    loss_db: 0
  - from: bsg.vhf
    to:   rdg.vhf
    loss_db: 0
```

The modem block is its own object so it can grow. Modes that need
parameters get them inside `modem:`:

```yaml
modem:
  mode: bpsk
  baud: 1200
  carrier_hz: 1500

modem:
  mode: il2p
  inner: gfsk9600
  crc: true
```

Future-proofing rule: **unknown fields inside `modem:` are an error**, not
silently ignored. A typo (`baud_rate` instead of `baud`) should fail loudly
at startup so users don't run with non-default modem configs they didn't
realise they wrote.

Acceptance: 2-node single-port-each config, send a KISS frame to node A's
KISS port via `nc localhost 8001`, observe it arrive on node B's KISS port.
Plus a 1-node 2-port config validates and starts cleanly. Plus a config
with a deliberately mismatched-modem link fails to start with a
human-readable error.

### Phase 3 — Self-mute, attenuation, FM capture-effect mixing

The simulator gets useful here. **Read this phase carefully — the mixing
model is not what you'd reach for by default.**

Note the mixer operates **per port**, not per node. A node's two ports
have independent audio paths that don't interact (which matches reality —
they're different radios on different frequencies). Self-mute means the
keying *port* doesn't hear itself; the same node's other ports can
receive normally during the keying.

1. **Self-mute**: detect TX-in-progress by reading the audio stream — any
   non-silence indicates the port is keying. While a port is keying, route
   its TX audio to peers but *not* back to itself. Add a small hold-off
   (~100 ms of silence required to consider TX ended) so brief intra-frame
   silences don't unmute prematurely.

2. **Attenuation**: each link has a `loss_db`. The receiver-side level for
   a given TX is `-loss_db` (more negative = weaker). Sample scaling for
   the actual audio is `10^(-loss_db/20)`, but the *level* used for the
   capture decision below is just the dB value itself.

3. **FM capture-effect mixing (this is the important bit)**: pure linear
   sum mixing is wrong for FM. Real FM receivers exhibit the *capture
   effect* — when two FM signals arrive simultaneously, the stronger one
   takes over the demodulator and the weaker one is suppressed, provided
   the level difference exceeds a threshold (the capture ratio, typically
   6 dB for narrow-band 2m FM).

   Implement the receiver-side mixer as follows:

   ```
   For each RX bus (per port) every audio block:
     active := list of TX streams currently keying that reach this port
     if len(active) == 0:    output silence
     if len(active) == 1:    output that stream, attenuated per loss_db
     if len(active) >= 2:
       sort active by RX level (descending — strongest first)
       margin := active[0].rx_level_db - active[1].rx_level_db
       if margin >= capture_db:
         output active[0], attenuated   // capture: strongest wins
       else:
         output collision garbage       // mutual destruction
   ```

   `capture_db` is the global config setting introduced in Phase 2,
   default **6.0 dB**.

   "Collision garbage" should be: zero samples (silence) for the simplest
   defensible model. A real FM receiver under collision produces a noisy
   muddle, but for AX.25 link-layer testing what matters is "neither frame
   decodes" and silence achieves that without introducing a noise model
   that complicates analysis. The `collision_mode: silence | sum | noise`
   config selects between these; v1 only needs `silence` working,
   the others can be stubs that log "not implemented."

4. **Linear sum mode**: keep linear-sum mixing as a non-default option for
   future SSB modelling, selected via `mixer_mode: linear_sum`. v1 just
   needs the FM path working; `linear_sum` can be stubbed.

Acceptance: 3-node hidden-node topology (A↔B and B↔C, no A↔C). Configure
A→B and C→B with equal loss. A and C transmit overlapping frames at B; B
sees collisions and decodes neither cleanly (capture margin = 0, falls
into collision branch). Then re-run with A→B at -3 dB and C→B at -15 dB:
A's frames now punch through cleanly even when C is keying, because A
captures (margin = 12 dB ≥ 6 dB). Demonstrate both behaviours from the
same code, configuration-driven.

### Phase 4 — Noise injection

Per-link optional white noise generator. Configurable in YAML as `noise_db`
per link. Sum the noise samples into the RX bus alongside routed TX audio.

Note this is independent of the capture-effect mixer — noise is added at
the audio level after the capture decision has been made. (In real life,
high noise would degrade the captured signal's decode rate, which is the
behaviour we want to see emerge.)

Reasonable approach: pre-generate a buffer of normally-distributed noise
samples scaled to the configured level, loop it. Don't try to be clever
about spectral shaping — flat white noise is the right model for
"channel noise floor" at this layer.

Acceptance: with noise turned up to a level that meaningfully reduces SNR,
frame error rate increases. Demonstrate by counting frames-in vs
frames-decoded over a fixed-duration test run with and without noise.

### Phase 5 — Demo topologies

`/opt/sim/configs/`:
- `hidden-node.yaml` — A↔B↔C, no A↔C, equal levels (mutual destruction).
  All ports use AFSK1200.
- `hidden-node-capture.yaml` — A↔B↔C, no A↔C, A much louder at B than C
  (A captures, C never gets through). Same hidden-node topology, very
  different network behaviour. Worth having both as separate demos to
  illustrate the capture-effect model.
- `mesh-3.yaml` — 3 nodes fully connected, AFSK1200.
- `linear-6.yaml` — 6-node chain, each hears only immediate neighbours,
  AFSK1200.
- `star-6.yaml` — 6 nodes around central hub, AFSK1200.
- `multiport-3.yaml` — 3 nodes, the middle one with two ports: a VHF
  AFSK1200 port to one peer and a UHF GFSK9600 port to the other.
  Demonstrates that multi-port nodes work and that different-modem
  ports on the same node coexist independently.

Each gets a corresponding `make demo-<name>` target that starts the router
with that config and prints the relevant ports. Brief usage notes per
demo in the main README.

If samoyed currently doesn't support GFSK9600, drop `multiport-3.yaml` to
two AFSK1200 ports per the multi-port node — the demonstration is about
port multiplicity, not about modem variety. Note the workaround in the
demo README and revisit when samoyed gains the missing mode.

Acceptance: `make demo-hidden-node` brings up the rig, README explains
exactly how to drive it from another shell to observe collisions.
`make demo-multiport-3` brings up a multi-port node and demonstrates the
two ports operating independently.

## Decisions already made — don't relitigate

- **Language: Go.** Matches samoyed; single binary; good concurrency
  primitives for audio handling.
- **Config: YAML.** Standard, hand-editable. Use `gopkg.in/yaml.v3` with
  strict mode (`KnownFields(true)` on the decoder) so unknown fields
  fail loudly.
- **Audio format: mono / 48 kHz / signed 16-bit LE** unless Phase 1
  reveals samoyed wants something different.
- **Transport: TCP** for audio between router and samoyed. Localhost only.
- **Mixing model: FM capture effect (default).** See Phase 3. Linear
  sum exists as a stub for future SSB work but is not the default.
- **Capture ratio: 6 dB** default, configurable.
- **Modem model: per-port, not per-node or per-link.** A port has one
  modem; a link's two endpoints must agree on modem config. The router
  is DSP-unaware — modem config is opaque pass-through to samoyed plus
  link-level compatibility validation.
- **KISS exposure: TCP per port**, configurable port number. Router does
  not touch KISS frames — samoyed handles that, router just exposes the
  port.
- **BPQ integration: out of scope for v1.** When v2 needs it, prebuilt
  Docker images are available: `m0lte/linbpq` on Docker Hub for LinBPQ,
  `ghcr.io/packethacking/xrouter` for XRouter. Either can be pointed at
  the router's localhost KISS ports without modification to the sim.
- **No persistence, no web UI, no API, no hot reload.** Single config
  file, restart to reload.
- **Single process, single host.** No distributed routing.

## Out of scope for v1

- Real-radio bridge (real soundcard I/O for hybrid hardware/sim testing)
- Hot reload of topology
- Web UI / topology visualisation
- Recording/playback of test runs
- BER/FER reporting beyond basic frame counters
- SSB modulation modelling (linear sum mixer, AGC effects)
- `linear_sum` and `noise` collision modes (stubs only — `silence` is
  enough for v1)
- BPQ/XRouter integration (images noted above for when v2 picks it up)
- Modem modes beyond what samoyed currently supports — YAML must accept
  the future modes per the catalogue, but the router refuses to start
  configs that request unsupported ones
- Anything DSP-flavoured beyond simple attenuation, noise, and the
  capture-effect mixer. No multipath, no Doppler, no fading, no
  pre/de-emphasis modelling.

## Workflow

- **Commit on master directly.** No feature branches.
- Commit at the end of each phase with a clear message — don't squash
  everything into one commit.
- `go vet` and `gofmt` before every commit. `go test ./...` must pass.
- Keep the router under ~1500 lines of Go for v1. If it's blowing past
  that, you're accumulating accidental complexity — stop and rethink.
- Minimal dependencies: stdlib + `yaml.v3`. No web frameworks, no logging
  frameworks, no DI containers. Use `log/slog` for structured logging.
- Log to stderr at INFO by default, DEBUG via `-v`.

## Deliverables

- `/opt/sim/cmd/sim-router/main.go` plus supporting packages in
  `/opt/sim/internal/`
- `/opt/sim/configs/*.yaml` — the demo topologies
- `/opt/sim/Makefile` — `build`, `test`, `demo-<name>`, `clean`
- `/opt/sim/README.md` — overview, quickstart, ASCII architecture diagram,
  brief explanation of the capture-effect mixer (this is non-obvious and
  someone reading the source for the first time will wonder), modem
  catalogue table with current samoyed support status
- `/opt/sim/NOTES-audio-io.md` — Phase 1 findings (audio I/O method, modem
  mode enumeration), kept for posterity
- `/opt/sim/go.mod`, `/opt/sim/go.sum`

## Hard rules

- **Don't modify samoyed.** It's a friend's project. If samoyed turns out
  to lack viable non-kernel audio I/O, fall back to stock Dire Wolf for v1
  and file an issue against samoyed describing what we'd need. Don't send
  a surprise PR.
- **Don't install pulseaudio, pipewire, jackd or any userspace sound
  daemon.** The whole point is no kernel/userspace audio stack. If you
  catch yourself reaching for one, you've taken a wrong turn.
- **Don't add a TUI/web UI/dashboard.** Logs to stderr, frame counters to
  stdout. That's the interface for v1.
- **Don't replace the FM capture-effect mixer with linear sum because it's
  simpler.** It is simpler. It is also wrong for the dominant use case
  (2m FM packet) and the whole point of the rig is realistic FM behaviour.
- **Don't make the router DSP-aware.** It pipes opaque PCM bytes between
  samoyed processes; samoyed does all modulation and demodulation. The
  modem field in the YAML is configuration to pass through, plus a
  compatibility check across link endpoints. If you find yourself wanting
  to add modulation logic to the router, stop — that's samoyed's job.
- **Stop and ask** if Phase 1 reveals samoyed has no usable non-kernel
  audio path *and* stock Dire Wolf doesn't either. That's a real blocker
  worth flagging up rather than working around.
