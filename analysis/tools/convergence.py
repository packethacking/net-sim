#!/usr/bin/env python3
"""Analyze routing convergence from monitor.py output."""

import argparse
import json
import sys


def analyze(snapshots_file):
    snapshots = []
    with open(snapshots_file) as f:
        for line in f:
            line = line.strip()
            if line and line.startswith("{"):
                try:
                    snapshots.append(json.loads(line))
                except json.JSONDecodeError:
                    continue

    if not snapshots:
        print("No data.", file=sys.stderr)
        return {}

    num_nodes = len(snapshots[0]["entries"])
    max_routes = num_nodes * (num_nodes - 1)

    first_convergence = None
    convergence_timeline = []

    for snap in snapshots:
        t = snap["elapsed_s"]
        total = snap["summary"]["total_nodes_known"]
        pct = snap["summary"]["pct_converged"]
        converged = snap["summary"]["fully_converged"]

        convergence_timeline.append({"t": t, "pct": pct, "total": total})

        if converged and first_convergence is None:
            first_convergence = t

    per_node_final = {}
    if snapshots:
        last = snapshots[-1]
        for entry in last["entries"]:
            routes = entry.get("nodes", [])
            per_node_final[entry["node_id"]] = {
                "known_nodes": len(routes),
                "routes": [
                    {
                        "dest": r["Call"],
                        "alias": r.get("Alias", ""),
                        "best_quality": max(
                            (rt["Quality"] for rt in r.get("Routes", [])), default=0
                        ),
                        "num_routes": len(r.get("Routes", [])),
                    }
                    for r in routes
                ],
            }

    observed = snapshots[-1].get("observed", {}) if snapshots else {}
    total_frames = sum(v.get("total_frames", 0) for v in observed.values())
    nodes_frames = sum(v.get("nodes_frames", 0) for v in observed.values())

    result = {
        "num_nodes": num_nodes,
        "max_routes": max_routes,
        "duration_s": snapshots[-1]["elapsed_s"] if snapshots else 0,
        "num_snapshots": len(snapshots),
        "convergence_time_s": first_convergence,
        "final_convergence_pct": snapshots[-1]["summary"]["pct_converged"]
        if snapshots
        else 0,
        "timeline": convergence_timeline,
        "per_node_final": per_node_final,
        "traffic": {
            "total_frames": total_frames,
            "nodes_frames": nodes_frames,
            "data_frames": total_frames - nodes_frames,
        },
    }
    return result


def print_report(result):
    print("=" * 60)
    print("NET/ROM CONVERGENCE ANALYSIS")
    print("=" * 60)
    print(f"Nodes: {result['num_nodes']}")
    print(f"Duration: {result['duration_s']:.0f}s")
    print(f"Snapshots: {result['num_snapshots']}")
    print()

    if result["convergence_time_s"] is not None:
        print(f"Full convergence at: {result['convergence_time_s']:.1f}s")
    else:
        print(f"NOT fully converged ({result['final_convergence_pct']:.1f}%)")

    print()
    print("Convergence timeline:")
    for pt in result["timeline"]:
        bar = "#" * int(pt["pct"] / 2)
        print(f"  {pt['t']:7.1f}s  {pt['pct']:5.1f}%  {bar}")

    print()
    print("Per-node final state:")
    for nid, ndata in sorted(result["per_node_final"].items()):
        known = ndata["known_nodes"]
        max_q = max((r["best_quality"] for r in ndata["routes"]), default=0)
        avg_q = (
            sum(r["best_quality"] for r in ndata["routes"]) / len(ndata["routes"])
            if ndata["routes"]
            else 0
        )
        multi_route = sum(1 for r in ndata["routes"] if r["num_routes"] > 1)
        print(
            f"  {nid}: {known} nodes known, "
            f"best_q={max_q}, avg_q={avg_q:.0f}, "
            f"multi_route={multi_route}"
        )

    print()
    t = result["traffic"]
    print(f"Traffic: {t['total_frames']} total, {t['nodes_frames']} NODES, {t['data_frames']} data")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "input",
        nargs="?",
        default="analysis/results/routing_snapshots.jsonl",
        help="Input JSONL file from monitor.py",
    )
    parser.add_argument("-o", "--output", help="Output JSON summary")
    args = parser.parse_args()

    result = analyze(args.input)
    print_report(result)

    if args.output:
        with open(args.output, "w") as f:
            json.dump(result, f, indent=2)
        print(f"\nSaved to {args.output}", file=sys.stderr)


if __name__ == "__main__":
    main()
