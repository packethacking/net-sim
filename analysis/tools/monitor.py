#!/usr/bin/env python3
"""Poll LinBPQ nodes for routing tables and record convergence data."""

import argparse
import json
import sys
import time
import urllib.request
import urllib.error
from datetime import datetime, timezone


NODES = [
    {"id": f"n{i}", "call": f"SIM{i}", "http": 8200 + i}
    for i in range(1, 11)
]


def fetch_json(url, timeout=3):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
            raw = raw.replace("\r\n", "\n").strip()
            return json.loads(raw)
    except (urllib.error.URLError, OSError, json.JSONDecodeError, ValueError):
        return None


def poll_node(node):
    base = f"http://127.0.0.1:{node['http']}"
    nodes_data = fetch_json(f"{base}/api/nodes")
    routes_data = fetch_json(f"{base}/api/routes")
    info_data = fetch_json(f"{base}/api/info")
    return {
        "node_id": node["id"],
        "call": node["call"],
        "nodes": nodes_data.get("nodes", []) if nodes_data else [],
        "routes": routes_data.get("routes", []) if routes_data else [],
        "info": info_data.get("info", {}) if info_data else {},
    }


def poll_netsim_observed():
    data = fetch_json("http://127.0.0.1:8080/api/observed")
    if not data:
        return {}
    result = {}
    for port_key, port_data in data.get("ports", {}).items():
        total_frames = sum(c["frames"] for c in port_data.get("calls", []))
        nodes_frames = sum(
            c["roles"].get("NODES", 0) for c in port_data.get("calls", [])
        )
        result[port_key] = {
            "total_frames": total_frames,
            "nodes_frames": nodes_frames,
        }
    return result


def snapshot(start_time):
    now = datetime.now(timezone.utc)
    elapsed = (now - start_time).total_seconds()
    entries = []
    for node in NODES:
        entries.append(poll_node(node))

    observed = poll_netsim_observed()

    total_known = 0
    max_possible = len(NODES) - 1
    fully_converged = True
    for entry in entries:
        known = len(entry["nodes"])
        total_known += known
        if known < max_possible:
            fully_converged = False

    return {
        "timestamp": now.isoformat(),
        "elapsed_s": round(elapsed, 1),
        "entries": entries,
        "observed": observed,
        "summary": {
            "total_nodes_known": total_known,
            "max_possible": max_possible * len(NODES),
            "pct_converged": round(
                100 * total_known / (max_possible * len(NODES)), 1
            ),
            "fully_converged": fully_converged,
        },
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "-i", "--interval", type=int, default=10, help="Poll interval (seconds)"
    )
    parser.add_argument(
        "-d", "--duration", type=int, default=600, help="Total duration (seconds)"
    )
    parser.add_argument(
        "-o",
        "--output",
        default="analysis/results/routing_snapshots.jsonl",
        help="Output file",
    )
    parser.add_argument("--console", action="store_true", help="Print summary to stderr")
    args = parser.parse_args()

    start_time = datetime.now(timezone.utc)
    end_time = start_time.timestamp() + args.duration

    with open(args.output, "w") as f:
        while time.time() < end_time:
            snap = snapshot(start_time)
            f.write(json.dumps(snap) + "\n")
            f.flush()

            if args.console:
                s = snap["summary"]
                per_node = []
                for e in snap["entries"]:
                    per_node.append(f"{e['node_id']}={len(e['nodes'])}")
                print(
                    f"[{snap['elapsed_s']:6.1f}s] "
                    f"{s['pct_converged']:5.1f}% converged "
                    f"({s['total_nodes_known']}/{s['max_possible']}) "
                    f"{'CONVERGED' if s['fully_converged'] else ''} "
                    f"  {' '.join(per_node)}",
                    file=sys.stderr,
                )

            time.sleep(args.interval)

    print(f"Done. {args.output} written.", file=sys.stderr)


if __name__ == "__main__":
    main()
