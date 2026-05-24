#!/usr/bin/env python3
"""Run a single NET/ROM experiment with a specified LinBPQ image variant."""

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.request
import urllib.error


def fetch_json(url, timeout=3):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
            raw = raw.replace("\r\n", "\n").strip()
            return json.loads(raw)
    except (urllib.error.URLError, OSError, json.JSONDecodeError, ValueError):
        return None


def wait_for_netsim(timeout=120):
    start = time.time()
    while time.time() - start < timeout:
        data = fetch_json("http://127.0.0.1:8080/api/status")
        if data and data.get("running"):
            return True
        time.sleep(2)
    return False


def wait_for_nodes(timeout=60):
    start = time.time()
    while time.time() - start < timeout:
        ready = 0
        for i in range(1, 11):
            data = fetch_json(f"http://127.0.0.1:{8200 + i}/api/info")
            if data and data.get("info", {}).get("NodeCall"):
                ready += 1
        if ready >= 8:
            return ready
        time.sleep(3)
    return ready


def poll_convergence(duration=600, interval=15):
    """Poll routing tables and return convergence timeline."""
    start_time = time.time()
    timeline = []
    converged_at = None

    while time.time() - start_time < duration:
        total_known = 0
        per_node = {}
        for i in range(1, 11):
            nid = f"n{i}"
            data = fetch_json(f"http://127.0.0.1:{8200 + i}/api/nodes")
            count = 0
            if data and data.get("nodes"):
                count = len(data["nodes"])
            per_node[nid] = count
            total_known += count

        elapsed = time.time() - start_time
        pct = 100 * total_known / 90
        fully = total_known >= 90

        timeline.append({
            "t": round(elapsed, 1),
            "pct": round(pct, 1),
            "total": total_known,
            "per_node": per_node,
        })

        if fully and converged_at is None:
            converged_at = round(elapsed, 1)

        print(
            f"  [{elapsed:6.1f}s] {pct:5.1f}% ({total_known}/90)"
            f" {'CONVERGED' if fully else ''}",
            file=sys.stderr,
        )

        if fully and elapsed > converged_at + 120:
            break

        time.sleep(interval)

    return timeline, converged_at


def poll_observed():
    data = fetch_json("http://127.0.0.1:8080/api/observed")
    if not data:
        return 0, 0
    total = 0
    nodes_f = 0
    for port_data in data.get("ports", {}).values():
        for call in port_data.get("calls", []):
            total += call.get("frames", 0)
            nodes_f += call.get("roles", {}).get("NODES", 0)
    return total, nodes_f


def run_experiment(variant, image, compose_file, results_dir):
    os.makedirs(results_dir, exist_ok=True)

    print(f"\n{'=' * 60}", file=sys.stderr)
    print(f"EXPERIMENT: {variant} ({image})", file=sys.stderr)
    print(f"{'=' * 60}", file=sys.stderr)

    # Bring down any existing stack
    subprocess.run(
        ["docker", "compose", "-f", compose_file, "down", "--remove-orphans"],
        capture_output=True,
    )
    time.sleep(2)

    # Clean LinBPQ state
    for i in range(1, 11):
        node_dir = os.path.join(os.path.dirname(compose_file), "nodes", f"n{i}")
        for f in ["BPQNODES.dat", "MHSave.txt"]:
            path = os.path.join(node_dir, f)
            if os.path.exists(path):
                os.remove(path)

    # Update compose file to use the variant image
    with open(compose_file) as f:
        compose = f.read()

    tmp_compose = compose_file + f".{variant}.tmp"
    with open(tmp_compose, "w") as f:
        f.write(compose.replace("m0lte/linbpq:latest", image))

    try:
        # Start stack
        print("Starting containers...", file=sys.stderr)
        result = subprocess.run(
            ["docker", "compose", "-f", tmp_compose, "up", "-d"],
            capture_output=True,
            text=True,
            timeout=120,
        )
        if result.returncode != 0:
            print(f"Failed to start: {result.stderr}", file=sys.stderr)
            return None

        # Wait for net-sim
        print("Waiting for net-sim...", file=sys.stderr)
        if not wait_for_netsim():
            print("net-sim failed to start", file=sys.stderr)
            return None

        # Stagger LinBPQ node restarts to avoid synchronized NODES broadcasts
        print("Staggering node restarts...", file=sys.stderr)
        for i in range(1, 11):
            subprocess.run(
                ["docker", "restart", f"analysis-n{i}-1"],
                capture_output=True,
                timeout=15,
            )
            time.sleep(3)

        # Wait for LinBPQ nodes
        print("Waiting for LinBPQ nodes...", file=sys.stderr)
        ready = wait_for_nodes(timeout=90)
        print(f"  {ready}/10 nodes ready", file=sys.stderr)

        time.sleep(10)

        # Run convergence test
        print("Running convergence test...", file=sys.stderr)
        timeline, converged_at = poll_convergence(duration=600, interval=15)

        # Collect final observed traffic
        total_frames, nodes_frames = poll_observed()

        # Collect final routing quality
        route_quality = {}
        for i in range(1, 11):
            nid = f"n{i}"
            data = fetch_json(f"http://127.0.0.1:{8200 + i}/api/nodes")
            if data and data.get("nodes"):
                qualities = [
                    max(r["Quality"] for r in n.get("Routes", [{"Quality": 0}]))
                    for n in data["nodes"]
                ]
                route_quality[nid] = {
                    "known": len(data["nodes"]),
                    "avg_quality": round(sum(qualities) / len(qualities), 1)
                    if qualities
                    else 0,
                    "max_quality": max(qualities) if qualities else 0,
                }

        result = {
            "variant": variant,
            "image": image,
            "convergence_time_s": converged_at,
            "final_convergence_pct": timeline[-1]["pct"] if timeline else 0,
            "total_frames": total_frames,
            "nodes_frames": nodes_frames,
            "data_frames": total_frames - nodes_frames,
            "nodes_pct_of_total": round(
                100 * nodes_frames / total_frames if total_frames else 0, 1
            ),
            "route_quality": route_quality,
            "timeline": timeline,
        }

        out_path = os.path.join(results_dir, f"{variant}.json")
        with open(out_path, "w") as f:
            json.dump(result, f, indent=2)

        print(f"\nResults saved to {out_path}", file=sys.stderr)
        print(f"  Convergence: {converged_at}s", file=sys.stderr)
        print(
            f"  Traffic: {total_frames} total, {nodes_frames} NODES ({result['nodes_pct_of_total']}%)",
            file=sys.stderr,
        )

        return result

    finally:
        # Clean up
        subprocess.run(
            ["docker", "compose", "-f", tmp_compose, "down", "--remove-orphans"],
            capture_output=True,
        )
        os.remove(tmp_compose)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("variant", help="Variant name (baseline, split-horizon, etc.)")
    parser.add_argument(
        "--image",
        default=None,
        help="Docker image (default: linbpq-test:<variant>)",
    )
    parser.add_argument(
        "--compose",
        default="analysis/docker-compose.yml",
        help="Compose file",
    )
    parser.add_argument(
        "--results-dir",
        default="analysis/results/experiments",
        help="Results directory",
    )
    args = parser.parse_args()

    image = args.image or f"linbpq-test:{args.variant}"
    run_experiment(args.variant, image, args.compose, args.results_dir)


if __name__ == "__main__":
    main()
