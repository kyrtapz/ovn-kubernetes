#!/usr/bin/env python3
"""
Analyze OVN-Kubernetes telemetry JSON logs.

Primary analysis: timestamp diffs between sequential events per pod+network.
Secondary: self-reported elapsed_ms for sanity checking.

Usage:
  # From oc adm inspect output:
  ./analyze-telemetry.py --inspect-dir /path/to/inspect.local.XXX

  # From live cluster:
  ./analyze-telemetry.py --kubeconfig /path/to/admin.conf

  # From stdin (piped JSON lines):
  cat telemetry.jsonl | ./analyze-telemetry.py --stdin
"""

import argparse
import gzip
import json
import subprocess
import sys
from collections import defaultdict
from datetime import datetime
from pathlib import Path

import numpy as np

STEP_ORDER = [
    "cm_pod_received",
    "cm_annotated",
    "ctrl_pod_received",
    "ctrl_lsp_created",
    "cni_add_start",
    "cni_annotation_ready",
    "cni_interface_configured",
    "cni_ovs_port_added",
    "cni_ovn_installed",
    "cni_complete",
    "eps_mirrored",
    "svc_lb_updated",
    "ctrl_netpol_pod_added",
]

# Step pairs for timestamp-diff analysis.
# (from_event, to_event, label)
# Within same (pod, network) timeline unless cross_network is set.
STEP_PAIRS = [
    # Cluster manager
    ("cm_pod_received", "cm_annotated", "CM: pod received → annotated"),
    # CM → Controller handoff
    ("cm_annotated", "ctrl_pod_received", "CM → Controller informer lag"),
    # Controller
    ("ctrl_pod_received", "ctrl_lsp_created", "Controller: received → LSP created"),
    # CNI sequential steps
    ("cni_add_start", "cni_annotation_ready", "CNI: start → annotation ready"),
    ("cni_annotation_ready", "cni_interface_configured", "CNI: annotation → interface configured"),
    ("cni_interface_configured", "cni_ovs_port_added", "CNI: interface → OVS port added"),
    ("cni_ovs_port_added", "cni_ovn_installed", "CNI: OVS port → ovn-installed"),
    # CNI totals
    ("cni_add_start", "cni_complete", "CNI: total (start → complete)"),
]


def parse_ts(ts_str):
    if "." in ts_str:
        base, frac = ts_str.split(".")
        frac = frac.rstrip("Z")
        frac = frac[:6]
        ts_str = f"{base}.{frac}Z"
    return datetime.fromisoformat(ts_str.replace("Z", "+00:00"))


# ── Collection ──────────────────────────────────────────────────────────────

def collect_from_cluster(kubeconfig):
    env = {"KUBECONFIG": kubeconfig, "PATH": "/usr/bin:/usr/local/bin:/bin:/usr/local/sbin"}
    events = []

    result = subprocess.run(
        ["kubectl", "get", "pods", "-n", "ovn-kubernetes", "--no-headers",
         "-o", "custom-columns=NAME:.metadata.name"],
        capture_output=True, text=True, env=env
    )
    pod_names = [p.strip() for p in result.stdout.strip().split("\n") if p.strip()]

    for pod_name in pod_names:
        containers = []
        if "control-plane" in pod_name:
            containers = [None]
        elif "ovnkube-node" in pod_name:
            containers = ["ovnkube-controller"]
        else:
            continue

        for container in containers:
            cmd = ["kubectl", "logs", "-n", "ovn-kubernetes", pod_name]
            if container:
                cmd.extend(["-c", container])

            result = subprocess.run(cmd, capture_output=True, text=True, env=env)
            events.extend(_extract_json_lines(result.stdout.split("\n"), pod_name))

    return events


def _extract_json_lines(lines, source=""):
    events = []
    for line in lines:
        if isinstance(line, bytes):
            line = line.decode("utf-8", errors="replace")
        line = line.strip()
        idx = line.find("{")
        if idx < 0:
            continue
        candidate = line[idx:]
        if '"event"' not in candidate:
            continue
        try:
            evt = json.loads(candidate)
            evt["_source"] = source
            events.append(evt)
        except json.JSONDecodeError:
            pass
    return events


TELEMETRY_CONTAINERS = {"ovnkube-controller", "ovnkube-cluster-manager"}


def collect_from_inspect(inspect_dir):
    events = []
    inspect_path = Path(inspect_dir)

    ns_dirs = []
    namespaces_dir = inspect_path / "namespaces"
    if namespaces_dir.exists():
        for d in namespaces_dir.iterdir():
            if d.is_dir() and "ovn" in d.name:
                ns_dirs.append(d)
    if not ns_dirs:
        pods_dir = inspect_path / "pods"
        if pods_dir.exists():
            ns_dirs = [inspect_path]

    for ns_dir in ns_dirs:
        pods_dir = ns_dir / "pods"
        if not pods_dir.exists():
            continue

        for pod_dir in pods_dir.iterdir():
            if not pod_dir.is_dir():
                continue
            if not any(x in pod_dir.name for x in ("ovnkube-node", "ovnkube-control-plane")):
                continue

            for container_dir in pod_dir.iterdir():
                if not container_dir.is_dir():
                    continue
                if container_dir.name not in TELEMETRY_CONTAINERS:
                    continue

                logs_dir = container_dir / container_dir.name / "logs"
                if not logs_dir.exists():
                    continue

                source = f"{pod_dir.name}/{container_dir.name}"

                for log_name in ("current.log", "previous.log"):
                    log_file = logs_dir / log_name
                    if log_file.exists() and log_file.stat().st_size > 0:
                        with open(log_file) as fh:
                            events.extend(_extract_json_lines(fh, source))

                rotated_dir = logs_dir / "rotated"
                if rotated_dir.exists():
                    for rotated_file in sorted(rotated_dir.iterdir()):
                        if rotated_file.name.endswith(".gz"):
                            with gzip.open(rotated_file, "rt", errors="replace") as fh:
                                events.extend(_extract_json_lines(fh, source))
                        elif rotated_file.is_file() and "log" in rotated_file.name:
                            with open(rotated_file) as fh:
                                events.extend(_extract_json_lines(fh, source))

    return events


def collect_from_stdin():
    events = []
    for line in sys.stdin:
        line = line.strip()
        idx = line.find("{")
        if idx >= 0 and '"event"' in line[idx:]:
            try:
                events.append(json.loads(line[idx:]))
            except json.JSONDecodeError:
                pass
    return events


# ── Correlation ─────────────────────────────────────────────────────────────

def correlate_events(events):
    pods = defaultdict(list)
    network_events = []

    for evt in events:
        pod = evt.get("pod", "")
        network = evt.get("network", "")

        if pod:
            pods[(pod, network)].append(evt)
        else:
            network_events.append(evt)

    for key in pods:
        pods[key].sort(key=lambda e: e.get("ts", ""))

    return pods, network_events


# ── Analysis ────────────────────────────────────────────────────────────────

def compute_timestamp_deltas(pods):
    """PRIMARY: compute time between step pairs using event timestamps."""
    deltas = defaultdict(list)
    per_pod_timelines = {}

    for (pod, network), events in pods.items():
        ts_by_step = {}
        detail_by_step = {}
        for evt in events:
            step = evt.get("event", "")
            if step in STEP_ORDER:
                ts_by_step[step] = parse_ts(evt["ts"])
                detail_by_step[step] = evt.get("detail", {})

        per_pod_timelines[(pod, network)] = (ts_by_step, detail_by_step)

        # Queue latency from detail.scheduled_at
        for step in ("cm_pod_received", "ctrl_pod_received"):
            if step in ts_by_step and step in detail_by_step:
                sched = detail_by_step[step].get("scheduled_at")
                if sched:
                    sched_ts = parse_ts(sched)
                    q_ms = (ts_by_step[step] - sched_ts).total_seconds() * 1000
                    if q_ms >= 0:
                        deltas[f"Queue: scheduled → {step}"].append(q_ms)

        # Step pairs
        for start_step, end_step, label in STEP_PAIRS:
            if start_step in ts_by_step and end_step in ts_by_step:
                delta_ms = (ts_by_step[end_step] - ts_by_step[start_step]).total_seconds() * 1000
                if delta_ms >= 0:
                    deltas[label].append(delta_ms)

        # End-to-end
        if ts_by_step:
            ordered = sorted(ts_by_step.values())
            e2e_ms = (ordered[-1] - ordered[0]).total_seconds() * 1000
            deltas["End-to-end (first → last event)"].append(e2e_ms)

    # Cross-network: scheduled_at → cni_add_start
    # scheduled_at lives in ctrl_pod_received (any network), cni_add_start is on default network
    pods_by_name = defaultdict(dict)
    for (pod, network), (ts_by_step, detail_by_step) in per_pod_timelines.items():
        pods_by_name[pod][network] = (ts_by_step, detail_by_step)

    for pod, networks in pods_by_name.items():
        # Find scheduled_at from any network's ctrl_pod_received
        sched_ts = None
        for network, (ts_map, detail_map) in networks.items():
            if "ctrl_pod_received" in detail_map:
                sched = detail_map["ctrl_pod_received"].get("scheduled_at")
                if sched:
                    sched_ts = parse_ts(sched)
                    break

        if sched_ts is None:
            continue

        # Find cni_add_start (on default network)
        for network, (ts_map, _) in networks.items():
            if "cni_add_start" in ts_map:
                q_ms = (ts_map["cni_add_start"] - sched_ts).total_seconds() * 1000
                if q_ms >= 0:
                    deltas["Queue: scheduled → cni_add_start"].append(q_ms)
                break

    return deltas, per_pod_timelines


def compute_elapsed_percentiles(events):
    """SECONDARY: self-reported elapsed_ms per event type."""
    elapsed_by_type = defaultdict(list)
    for evt in events:
        event_type = evt.get("event", "")
        elapsed = evt.get("elapsed_ms")
        if elapsed is not None and elapsed > 0:
            elapsed_by_type[event_type].append(elapsed)
    return elapsed_by_type


# ── Output ──────────────────────────────────────────────────────────────────

def print_percentile_table(title, data_dict, sort_by_p99=True):
    if not data_dict:
        print(f"\n{title}: no data")
        return

    rows = []
    for label, values in data_dict.items():
        if not values:
            continue
        arr = np.array(values)
        rows.append({
            "label": label,
            "count": len(values),
            "p50": np.percentile(arr, 50),
            "p95": np.percentile(arr, 95),
            "p99": np.percentile(arr, 99),
            "max": np.max(arr),
        })

    if sort_by_p99:
        rows.sort(key=lambda r: r["p99"], reverse=True)

    print(f"\n{'=' * 110}")
    print(f"  {title}")
    print(f"{'=' * 110}")
    print(f"  {'Step':<50} {'Count':>6} {'P50 ms':>10} {'P95 ms':>10} {'P99 ms':>10} {'Max ms':>10}")
    print(f"  {'-' * 50} {'-' * 6} {'-' * 10} {'-' * 10} {'-' * 10} {'-' * 10}")
    for r in rows:
        print(f"  {r['label']:<50} {r['count']:>6} {r['p50']:>10.1f} {r['p95']:>10.1f} {r['p99']:>10.1f} {r['max']:>10.1f}")


def print_worst_pods(per_pod_timelines, n=10):
    e2e = []
    for (pod, network), (ts_by_step, _) in per_pod_timelines.items():
        if ts_by_step:
            ordered = sorted(ts_by_step.values())
            e2e_ms = (ordered[-1] - ordered[0]).total_seconds() * 1000
            e2e.append((e2e_ms, pod, network, ts_by_step))

    e2e.sort(reverse=True)

    print(f"\n{'=' * 110}")
    print(f"  Top {n} slowest pods (timestamp diffs between consecutive events)")
    print(f"{'=' * 110}")

    for i, (ms, pod, network, ts_by_step) in enumerate(e2e[:n]):
        print(f"\n  #{i + 1}: {pod} network={network} total={ms:.1f}ms")
        ordered_steps = sorted(ts_by_step.items(), key=lambda x: x[1])
        for j, (step, ts) in enumerate(ordered_steps):
            if j > 0:
                delta = (ts - ordered_steps[j - 1][1]).total_seconds() * 1000
                print(f"    {step:<35} +{delta:>8.1f}ms  ({ts.strftime('%H:%M:%S.%f')[:-3]})")
            else:
                print(f"    {step:<35} {'':>9}   ({ts.strftime('%H:%M:%S.%f')[:-3]})")


def print_event_counts(events):
    counts = defaultdict(int)
    for evt in events:
        counts[evt.get("event", "unknown")] += 1

    print(f"\n{'=' * 110}")
    print(f"  Event counts")
    print(f"{'=' * 110}")
    for event_type, count in sorted(counts.items(), key=lambda x: -x[1]):
        print(f"  {event_type:<50} {count:>6}")


def print_network_events(network_events):
    by_type = defaultdict(list)
    for evt in network_events:
        elapsed = evt.get("elapsed_ms", 0)
        if elapsed > 0:
            by_type[evt.get("event", "")].append(elapsed)

    if by_type:
        print_percentile_table("Network-level events (elapsed_ms, not per-pod)", by_type)


# ── Main ────────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(description="Analyze OVN-K telemetry logs")
    parser.add_argument("--inspect-dir", help="Path to oc adm inspect output directory")
    parser.add_argument("--kubeconfig", help="Path to kubeconfig for live cluster")
    parser.add_argument("--stdin", action="store_true", help="Read from stdin")
    parser.add_argument("--top", type=int, default=10, help="Number of worst pods to show")
    parser.add_argument("--filter-ns", help="Only include pods in namespaces matching this prefix")
    args = parser.parse_args()

    if args.inspect_dir:
        print(f"Collecting from inspect dir: {args.inspect_dir}")
        events = collect_from_inspect(args.inspect_dir)
    elif args.kubeconfig:
        print(f"Collecting from cluster: {args.kubeconfig}")
        events = collect_from_cluster(args.kubeconfig)
    elif args.stdin:
        events = collect_from_stdin()
    else:
        parser.print_help()
        sys.exit(1)

    if not events:
        print("No telemetry events found.")
        sys.exit(1)

    print(f"Collected {len(events)} events.")

    if args.filter_ns:
        prefix = args.filter_ns
        before = len(events)
        events = [e for e in events if e.get("pod", "").startswith(prefix) or not e.get("pod")]
        print(f"Filtered to ns prefix '{prefix}': {before} → {len(events)} events")

    print_event_counts(events)

    pods, network_events = correlate_events(events)
    print(f"\nCorrelated into {len(pods)} unique (pod, network) timelines.")

    # PRIMARY: timestamp diffs between events
    deltas, per_pod_timelines = compute_timestamp_deltas(pods)
    print_percentile_table("TIMESTAMP DIFFS between steps (primary analysis)", deltas)

    # Top N slowest pods
    print_worst_pods(per_pod_timelines, n=args.top)

    # Network-level events
    print_network_events(network_events)

    # SECONDARY: self-reported elapsed_ms
    elapsed_by_type = compute_elapsed_percentiles(events)
    print_percentile_table("SELF-REPORTED elapsed_ms (secondary, for sanity check)", elapsed_by_type)


if __name__ == "__main__":
    main()
