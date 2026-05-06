# Phase 1 — Audio I/O findings

Goal: feed PCM into samoyed and read TX audio out without going through any
kernel sound stack. Source for everything below: the samoyed tree at
`/opt/samoyed` (commit `1ba1c88`, built 2026-05-06).

## Verdict

samoyed exposes two non-kernel audio paths that, combined, give us a complete
loop:

| Direction | Mechanism | samoyed config |
|---|---|---|
| Audio in (RX from peer) | stdin | `ADEVICE - <out>` or `ADEVICE stdin <out>` |
| Audio in (RX from peer) | UDP listen | `ADEVICE udp:<port> <out>` |
| Audio out (TX to peer) | UDP datagrams | `ADEVICE <in> udp:<host>:<port>` |

**Stock Dire Wolf 1.8.1 has stdin input but *no* UDP output yet** — it
falls through to ALSA when handed `udp:host:port`. For ports with
`tnc: direwolf`, the router instead writes a per-port `.asoundrc` that
defines an ALSA `file` plugin pointing at a FIFO, and points
`ADEVICE - <name>` at it. Direwolf writes raw PCM to the FIFO; the
router reads the FIFO. Loops audio in userspace just fine, but it's a
workaround we'll happily delete once Dire Wolf supports `udp:` output.

```
pcm.dwout {
  type file
  slave.pcm null
  file "<workdir>/dw-<node>-<port>/tx.fifo"
  format raw
}
```

These are inherited from Dire Wolf. The router is going to use **stdin in /
UDP out** for every samoyed instance. Picked stdin over UDP for input because
backpressure is well-defined (kernel pipe buffer flow controls the router) and
because the router can stop writing to drop a port out of the bus cleanly.

Audio format: **mono / 44100 Hz / signed 16-bit LE** (samoyed's
`DEFAULT_SAMPLES_PER_SEC = 44100`, `DEFAULT_NUM_CHANNELS = 1`,
`DEFAULT_BITS_PER_SAMPLE = 16`). The plan's tentative 48 kHz didn't survive
contact — samoyed defaults to 44.1 kHz and the gen_packets / atest test
fixtures all use 44.1 kHz. We follow samoyed's default rather than upsampling
ourselves.

Stock Dire Wolf 1.8.1 (`apt install direwolf`) was kept as a reference —
useful for A/B sanity checks — but is **not** used at runtime. It only links
against ALSA (no UDP audio output), so it can't be the audio sink in this
architecture.

## The PortAudio problem (and the workaround)

samoyed cgo-links libportaudio (the `gordonklaus/portaudio` Go binding wraps
PortAudio 19.7+git20260206 from the Ubuntu archive). On this LXC PortAudio's
PulseAudio host backend tries `pa_context_connect` and fails (no daemon, by
deliberate plan rule). In this build that failure is fatal:
`Pa_Initialize()` returns `paUnanticipatedHostError`, samoyed prints
"`Pointless to continue without audio device`" and exits — even though we
have no intention of opening any PortAudio stream (`ADEVICE - udp:...` is
pure stdin/UDP).

Confirmed by minimal cgo program:

```
$ go run main.go
Init err: PulseAudio_Initialize: Can't connect to server
HostApis err=PortAudio not initialized count=0
```

Fix: a 6-line `LD_PRELOAD` shim that no-ops `Pa_Initialize` /
`Pa_Terminate`. samoyed never actually calls any other PortAudio function on
this code path because the audio I/O type is decided as `STDIN` for input and
the UDP path for output; both bypass the PortAudio code path entirely.

```c
// /opt/sim/preload/pa_stub.c
typedef int PaError;
PaError Pa_Initialize(void) { return 0; }
PaError Pa_Terminate(void)  { return 0; }
```

Build:

```
gcc -shared -fPIC -o /usr/local/lib/libpa_stub.so /opt/sim/preload/pa_stub.c
```

This is the minimum incision that satisfies the rules. We're not modifying
samoyed (per the hard rule), not installing a sound daemon (per the hard
rule), and we're scoped tightly enough that any future PortAudio call from
samoyed would crash loudly rather than silently mis-behave — which is the
behaviour we want if the assumption ever drifts.

If/when samoyed gains a "no audio backend required" mode (or PortAudio is
initialised lazily), this shim disappears. Worth opening an upstream issue.

## Working pipeline

### One-shot decode test (Phase 1 acceptance)

```bash
# Generate 10 known frames at 1200 baud as WAV
samoyed-gen_packets -N 10 -o /tmp/g1200.wav

cat > /tmp/d.conf <<EOF
ADEVICE - udp:127.0.0.1:9999
ACHANNELS 1
CHANNEL 0
MYCALL N0CALL
MODEM 1200
KISSPORT 0
KISSPORT 8001
AGWPORT 0
DNSSD 0
EOF

# Strip 44-byte WAV header, feed raw PCM via stdin
( dd if=/tmp/g1200.wav bs=1 skip=44 status=none ) | \
  LD_PRELOAD=/usr/local/lib/libpa_stub.so \
  samoyed-direwolf -c /tmp/d.conf -t 0 -q d
```

Result: all 10 frames decoded (`WB2OSZ-15>TEST:...0001 of 0010` through
`...0010 of 0010`).

The WA8LMF *Track 1* TNC test recording is not bundled in the Debian
direwolf package on this LXC. `samoyed-gen_packets` produces deterministic
known-good signals at any supported baud, so we use that as the canonical
fixture instead.

### End-to-end loopback (TX of one samoyed → RX of another)

The full simulator topology in miniature: samoyed-A modulates a KISS frame to
UDP audio, a userspace bridge forwards the audio to samoyed-B's UDP input,
samoyed-B demodulates and emits a KISS frame on its TCP port.

```bash
# A: KISS in on 8201, audio out on UDP 7100
cat > /tmp/dA.conf <<EOF
ADEVICE - udp:127.0.0.1:7100
ACHANNELS 1
CHANNEL 0
MYCALL N0A
MODEM 1200
KISSPORT 0
KISSPORT 8201
AGWPORT 0
DNSSD 0
EOF

# B: audio in via UDP 7200, dummy audio out (we don't care), KISS on 8202
cat > /tmp/dB.conf <<EOF
ADEVICE udp:7200 udp:127.0.0.1:9999
ACHANNELS 1
CHANNEL 0
MYCALL N0B
MODEM 1200
KISSPORT 0
KISSPORT 8202
AGWPORT 0
DNSSD 0
EOF

# Bridge A's TX UDP → B's RX UDP
socat -u UDP-RECV:7100,reuseaddr UDP-DATAGRAM:127.0.0.1:7200 &

# Start B (RX side first so its UDP listener is up)
LD_PRELOAD=/usr/local/lib/libpa_stub.so samoyed-direwolf -c /tmp/dB.conf -t 0 -q d &

# Start A (eat /dev/zero on stdin → silence baseline)
cat /dev/zero | LD_PRELOAD=/usr/local/lib/libpa_stub.so samoyed-direwolf -c /tmp/dA.conf -t 0 -q d &

# Send a KISS frame to A → expect it to come out of B
nc 127.0.0.1 8201 < kiss_frame.bin
nc 127.0.0.1 8202    # see decoded KISS frame
```

Verified: `N0A-1>APDW01:HELLOFROMA` decoded by samoyed-B with the original
payload intact, KISS framing on B's TCP port correct.

## Per-port samoyed config (canonical)

The router will template this per port:

```
ADEVICE - udp:127.0.0.1:<rx_audio_udp_port>
ACHANNELS 1
CHANNEL 0
MYCALL N0<derived from node id>   # samoyed wants something here, never used by the simulator
KISSPORT 0                  # remove the default 8001
KISSPORT <kiss_tcp_port>    # the one we expose
AGWPORT 0                   # we don't use AGW
DNSSD 0                     # don't spam mDNS
```

Plus per-modem CLI flags (see modem-mode table below).

`KISSPORT 0` is essential: without it samoyed *also* opens port 8001 by
default, and a second instance fails to bind. Same applies to `AGWPORT`
which defaults to 8000.

## Modem mode enumeration

Tested against this samoyed binary:

| Catalogue `mode` | Status | Invocation | Notes |
|---|---|---|---|
| `afsk1200` | ✅ works | config: `MODEM 1200` (or just default) | Bell 202, classic 2m FM AFSK. Phase 1 acceptance verified. |
| `gfsk9600` | ✅ works | CLI: `-B 9600 -g` | K9NG/G3RUH 9600 baud GMSK. **Config-file `MODEM 9600 g3ruh` is silently ignored** — only the CLI form took effect in our testing. The router will pass `-B 9600 -g` rather than relying on the config directive. |
| `bpsk` | ❌ not supported | — | samoyed has 2400 QPSK (`-B 2400`) and 4800 8PSK (`-B 4800`); HF 300/1200 BPSK is not in the modem catalogue. Router must reject this mode at startup. |
| `il2p` | ⚠ partial | CLI: `-I 1` for IL2P+CRC, `-I 0` for IL2P (weaker FEC) | IL2P transmit is wrapped around the underlying modem via the `-I` flag. RX-side detection is automatic — no flag needed. The `inner` config key maps to the underlying `-B/-g` flags; `crc` maps to `-I 1` vs `-I 0`. |

### Other CLI flags that matter

- `-c <conf>` — config file (we always template one per port)
- `-t 0` — disable text colour (cleaner logs)
- `-q d` — suppress APRS-decode chatter (we don't care about APRS semantics
  in the simulator; the router only needs the audio + KISS plumbing)
- `-q h` — suppress per-frame "audio level" lines (drop in production; keep
  during dev to spot quiet/clipped TX)
- `--error-rate Rn` — receive-side frame clobber rate. Useful as a *future*
  alternative noise model; v1 uses analog noise injection per the plan.
- `--bit-error-rate n` — receiver BER. Ditto.

### `MODEM` config quirks observed

- `MODEM` after `CHANNEL` works for `1200` (default). Setting `MODEM 9600`
  or `MODEM 9600 g3ruh` does *not* switch the channel to G3RUH — the channel
  reports back as "1200 baud, AFSK" at startup. The CLI flags `-B 9600 -g`
  override this correctly. The router uses CLI flags exclusively for non-1200
  modems.
- `MODEM 2400` / `MODEM 4800` would in principle activate QPSK / 8PSK; not
  in the v1 catalogue, not tested.

## Translation table (mode → samoyed args)

This becomes the source of truth for `internal/samoyedcmd` (Phase 2):

```
afsk1200:   conf "MODEM 1200"           (no extra CLI)
gfsk9600:   CLI  "-B 9600 -g"
il2p:
  inner=afsk1200, fec=strong:  conf "MODEM 1200" + CLI "-I 1"
  inner=afsk1200, fec=weak:    conf "MODEM 1200" + CLI "-I 0"
  inner=gfsk9600, fec=strong:  CLI "-B 9600 -g -I 1"
  inner=gfsk9600, fec=weak:    CLI "-B 9600 -g -I 0"
bpsk:       refuse — not supported by current samoyed
```

samoyed's `-I {0,1}` controls Reed-Solomon parity-byte count, *not* the
optional 2-byte trailing CRC defined by the IL2P spec — that variant
("IL2P+CRC" / "IL2Pc") is not implemented in samoyed at all (verified
in `src/il2p_send.go` and `src/il2p_codec.go`: the encode path only
takes `max_fec`). `fec` was named `crc` originally; renamed because the
old name implied a feature samoyed doesn't have.

## Open issues to file upstream (don't fix here)

1. PortAudio's PulseAudio-backend init failure is fatal even when no
   PortAudio device is opened. Mitigation: lazy-init PortAudio only when
   the configured ADEVICE actually needs it.
2. `MODEM <baud>` with non-1200 baud in the config file appears not to
   activate the corresponding modem — the channel keeps the 1200 AFSK
   defaults. Workaround above is to pass `-B/-g/-I` on the CLI. May be a
   parser ordering bug.

Both go into samoyed's tracker as issues, not patches. (Hard rule: don't
touch samoyed source.)
