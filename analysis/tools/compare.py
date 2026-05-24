#!/usr/bin/env python3
"""Compare results from multiple NET/ROM experiment variants."""

import argparse
import json
import os
import sys


def load_results(results_dir):
    results = {}
    for fname in sorted(os.listdir(results_dir)):
        if fname.endswith(".json"):
            path = os.path.join(results_dir, fname)
            with open(path) as f:
                data = json.load(f)
            variant = data.get("variant", fname.replace(".json", ""))
            results[variant] = data
    return results


def print_comparison(results):
    if not results:
        print("No results found.", file=sys.stderr)
        return

    variants = list(results.keys())

    print("=" * 80)
    print("NET/ROM VARIANT COMPARISON")
    print("=" * 80)
    print()

    # Header
    header = f"{'Metric':<30}"
    for v in variants:
        header += f" {v:>14}"
    print(header)
    print("-" * len(header))

    # Convergence time
    row = f"{'Convergence time (s)':<30}"
    for v in variants:
        ct = results[v].get("convergence_time_s")
        row += f" {ct if ct is not None else 'N/A':>14}"
    print(row)

    # Final convergence %
    row = f"{'Final convergence (%)':<30}"
    for v in variants:
        row += f" {results[v].get('final_convergence_pct', 0):>13.1f}%"
    print(row)

    # Total frames
    row = f"{'Total frames':<30}"
    for v in variants:
        row += f" {results[v].get('total_frames', 0):>14}"
    print(row)

    # NODES frames
    row = f"{'NODES frames':<30}"
    for v in variants:
        row += f" {results[v].get('nodes_frames', 0):>14}"
    print(row)

    # NODES % of total
    row = f"{'NODES overhead (%)':<30}"
    for v in variants:
        row += f" {results[v].get('nodes_pct_of_total', 0):>13.1f}%"
    print(row)

    # Data frames
    row = f"{'Data frames (L2 link setup)':<30}"
    for v in variants:
        row += f" {results[v].get('data_frames', 0):>14}"
    print(row)

    # Average route quality
    row = f"{'Avg route quality':<30}"
    for v in variants:
        rq = results[v].get("route_quality", {})
        if rq:
            avg = sum(n.get("avg_quality", 0) for n in rq.values()) / len(rq)
            row += f" {avg:>14.1f}"
        else:
            row += f" {'N/A':>14}"
    print(row)

    print()

    # Per-variant details
    for v in variants:
        rq = results[v].get("route_quality", {})
        if rq:
            print(f"\n{v} route quality:")
            for nid, ndata in sorted(rq.items()):
                print(
                    f"  {nid}: {ndata['known']} nodes, "
                    f"avg_q={ndata['avg_quality']}, max_q={ndata['max_quality']}"
                )

    # Baseline comparison (if baseline exists)
    if "baseline" in results and len(results) > 1:
        print(f"\n{'=' * 80}")
        print("IMPROVEMENT vs BASELINE")
        print(f"{'=' * 80}")
        base = results["baseline"]
        for v in variants:
            if v == "baseline":
                continue
            r = results[v]
            print(f"\n{v}:")

            base_ct = base.get("convergence_time_s")
            r_ct = r.get("convergence_time_s")
            if base_ct and r_ct:
                diff = r_ct - base_ct
                print(f"  Convergence: {diff:+.1f}s ({r_ct}s vs {base_ct}s)")

            base_nf = base.get("nodes_frames", 0)
            r_nf = r.get("nodes_frames", 0)
            if base_nf > 0:
                pct = 100 * (r_nf - base_nf) / base_nf
                print(f"  NODES overhead: {pct:+.1f}% ({r_nf} vs {base_nf})")

            base_tf = base.get("total_frames", 0)
            r_tf = r.get("total_frames", 0)
            if base_tf > 0:
                pct = 100 * (r_tf - base_tf) / base_tf
                print(f"  Total traffic: {pct:+.1f}% ({r_tf} vs {base_tf})")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "results_dir",
        nargs="?",
        default="analysis/results/experiments",
        help="Directory with experiment JSON files",
    )
    parser.add_argument("-o", "--output", help="Output summary JSON")
    args = parser.parse_args()

    results = load_results(args.results_dir)
    print_comparison(results)

    if args.output:
        with open(args.output, "w") as f:
            json.dump(
                {v: {k: val for k, val in d.items() if k != "timeline"} for v, d in results.items()},
                f,
                indent=2,
            )


if __name__ == "__main__":
    main()
