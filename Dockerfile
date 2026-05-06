# Multi-stage build for the net-sim simulator (sim-router + sim-web).
#
# Builder stage: clones samoyed, compiles it; builds the Go binaries
# from this checkout; compiles the LD_PRELOAD shim.
#
# Runtime stage: a slim image carrying just the binaries, the
# samoyed-direwolf runtime libraries (libportaudio2 et al.), and the
# default two-node network at /etc/sim/network.yaml. Entrypoint is
# sim-web on :8080.
#
# Build:
#   docker build -t net-sim:dev .
#
# Run with the bundled two-node network:
#   docker run --rm -p 8080:8080 -p 8001:8001 -p 8002:8002 net-sim:dev
#
# Run with a custom network (any KISS port range):
#   docker run --rm --network=host \
#     -v ./my-network.yaml:/etc/sim/network.yaml \
#     net-sim:dev
#
# Published image: ghcr.io/packethacking/net-sim
# Tags: :main, :main-<sha>, :v<x.y.z>, :latest (on tagged release)

# ---- builder ------------------------------------------------------------
FROM golang:1.22-bookworm AS builder

ARG SAMOYED_REF=main

RUN apt-get update && apt-get install -y --no-install-recommends \
        git make pkg-config gcc libc6-dev \
        libudev-dev libhamlib-dev portaudio19-dev \
        libavahi-client-dev libbsd-dev libgps-dev libasound2-dev \
    && rm -rf /var/lib/apt/lists/*

# samoyed (pinned via SAMOYED_REF build arg; defaults to main)
RUN git clone --depth 1 --branch ${SAMOYED_REF} \
        https://github.com/doismellburning/samoyed.git /src/samoyed \
    && make -C /src/samoyed cmds

# net-sim — copy source and build.
WORKDIR /src/net-sim
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags="-s -w" -o /out/sim-router ./cmd/sim-router \
 && go build -ldflags="-s -w" -o /out/sim-web    ./cmd/sim-web \
 && gcc -shared -fPIC -O2 -o /out/libpa_stub.so preload/pa_stub.c

# ---- runtime ------------------------------------------------------------
FROM debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
        libportaudio2 libhamlib4 libudev1 libavahi-client3 \
        libbsd0 libgps28 libasound2 libjack-jackd2-0 libpulse0 \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/* /var/log/*

# binaries
COPY --from=builder /out/sim-router         /usr/local/bin/sim-router
COPY --from=builder /out/sim-web            /usr/local/bin/sim-web
COPY --from=builder /out/libpa_stub.so      /usr/local/lib/libpa_stub.so
COPY --from=builder /src/samoyed/dist/samoyed-direwolf /usr/local/bin/samoyed-direwolf

# default network — overridable via volume mount
COPY configs/two-node.yaml /etc/sim/network.yaml

# LD_PRELOAD is required so samoyed-direwolf doesn't try to bring up a
# PortAudio backend in a daemon-less container. See NOTES-audio-io.md.
ENV LD_PRELOAD=/usr/local/lib/libpa_stub.so

# Web UI; default config writes KISS ports 8001 and 8002. Custom configs
# may bind anywhere — use --network=host (or `-p ...`) accordingly.
EXPOSE 8080 8001 8002

# A non-root user for the running process. samoyed-direwolf doesn't need
# any capabilities once the LD_PRELOAD shim is in place.
RUN useradd --system --no-create-home --shell /usr/sbin/nologin sim
USER sim

ENTRYPOINT ["/usr/local/bin/sim-web", "-addr", ":8080", "-config", "/etc/sim/network.yaml"]
# Default to autostart so a one-line `docker run` produces a running
# topology on the bundled config. Override by passing your own args
# (e.g. `... net-sim:main -autostart=false`) if you want to drive
# Start/Stop from the web UI or API.
CMD ["-autostart"]
