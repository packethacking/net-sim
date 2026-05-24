#!/usr/bin/env python3
"""Monitor net-sim SSE event stream for channel utilization metrics."""

import argparse
import json
import sys
import time
import urllib.request
from datetime import datetime, timezone


def stream_events(url="http://127.0.0.1:8080/api/events", timeout=5):
    req = urllib.request.Request(url)
    resp = urllib.request.urlopen(req, timeout=timeout)
    buf = b""
    while True:
        chunk = resp.read(1)
        if not chunk:
            break
        buf += chunk
        if buf.endswith(b"\n\n"):
            for line in buf.decode(errors="replace").strip().split("\n"):
                if line.startswith("data: "):
                    try:
                        yield json.loads(line[6:])
                    except json.JSONDecodeError:
                        pass
            buf = b""


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "-d", "--duration", type=int, default=300, help="Monitor duration (seconds)"
    )
    parser.add_argument(
        "-o",
        "--output",
        default="analysis/results/channel_events.jsonl",
        help="Output file",
    )
    args = parser.parse_args()

    start = time.time()
    port_tx = {}
    port_stats = {}
    collisions = 0
    captures = 0
    events_count = 0

    with open(args.output, "w") as f:
        try:
            for event in stream_events():
                now = time.time()
                elapsed = now - start
                if elapsed > args.duration:
                    break

                events_count += 1
                etype = event.get("type", "")
                port = event.get("port", "")

                if port not in port_stats:
                    port_stats[port] = {
                        "tx_time": 0.0,
                        "tx_count": 0,
                        "collisions": 0,
                        "captures": 0,
                    }

                if etype == "tx_start":
                    port_tx[port] = now
                    port_stats[port]["tx_count"] += 1
                elif etype == "tx_end":
                    if port in port_tx:
                        dur = now - port_tx.pop(port)
                        port_stats[port]["tx_time"] += dur
                elif etype == "rx_decision":
                    decision = event.get("decision", "")
                    if decision == "collision":
                        collisions += 1
                        port_stats[port]["collisions"] += 1
                    elif decision == "capture":
                        captures += 1
                        port_stats[port]["captures"] += 1

                f.write(json.dumps(event) + "\n")

                if events_count % 100 == 0:
                    print(
                        f"[{elapsed:6.1f}s] events={events_count} "
                        f"collisions={collisions} captures={captures}",
                        file=sys.stderr,
                    )
        except (OSError, KeyboardInterrupt):
            pass

    total_elapsed = time.time() - start

    summary = {
        "duration_s": round(total_elapsed, 1),
        "total_events": events_count,
        "total_collisions": collisions,
        "total_captures": captures,
        "per_port": {},
    }
    for port, stats in sorted(port_stats.items()):
        utilization = stats["tx_time"] / total_elapsed if total_elapsed > 0 else 0
        summary["per_port"][port] = {
            "tx_time_s": round(stats["tx_time"], 2),
            "tx_count": stats["tx_count"],
            "utilization_pct": round(100 * utilization, 1),
            "collisions": stats["collisions"],
            "captures": stats["captures"],
        }

    with open(args.output.replace(".jsonl", "_summary.json"), "w") as f:
        json.dump(summary, f, indent=2)

    print(
        f"\nChannel utilization summary ({total_elapsed:.0f}s):",
        file=sys.stderr,
    )
    for port, s in sorted(summary["per_port"].items()):
        print(
            f"  {port}: {s['utilization_pct']:5.1f}% util, "
            f"{s['tx_count']} tx, {s['collisions']} col, {s['captures']} cap",
            file=sys.stderr,
        )
    print(f"\nSaved to {args.output}", file=sys.stderr)


if __name__ == "__main__":
    main()
