#!/usr/bin/env python3
"""Spike 1B metrics analysis.

Usage: analyze.py <config-dir> [window ...]

<config-dir> holds metrics/<unix-seconds>/*.prom.gz snapshots (collect.sh) and
markers.txt ("label unix-seconds" lines). Each window is a marker pair prefix,
e.g. "steady" (steady_start .. steady_end) or "burst_1000". Without windows,
every *_start/*_end pair is analysed. Writes <config-dir>/analysis.json and
prints a summary. Counters are differenced between the snapshot at or after
the window start and the snapshot at or before its end; gauges report the
mean and max of the snapshots inside the window.
"""

import gzip
import json
import math
import os
import re
import sys
from collections import defaultdict

LINE = re.compile(r'^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+([^\s]+)')
LABEL = re.compile(r'(\w+)="((?:[^"\\]|\\.)*)"')


def parse(path):
    out = {}
    if not os.path.exists(path):
        return out
    try:
        with gzip.open(path, "rt") as f:
            for line in f:
                if line.startswith("#"):
                    continue
                m = LINE.match(line)
                if not m:
                    continue
                labels = tuple(sorted(LABEL.findall(m.group(2) or "")))
                try:
                    out[(m.group(1), labels)] = float(m.group(3))
                except ValueError:
                    pass
    except (OSError, EOFError):
        pass
    return out


def load_snapshots(d):
    root = os.path.join(d, "metrics")
    snaps = []
    for ts in sorted(os.listdir(root), key=int):
        sd = os.path.join(root, ts)
        snap = {"t": int(ts)}
        for f in os.listdir(sd):
            snap[f.replace(".prom.gz", "")] = parse(os.path.join(sd, f))
        snaps.append(snap)
    return snaps


def markers(d):
    m = {}
    with open(os.path.join(d, "markers.txt")) as f:
        for line in f:
            k, v = line.split()
            m[k] = int(v)
    return m


def select(series, metric, **match):
    for (n, labels), v in series.items():
        if n != metric:
            continue
        ld = dict(labels)
        if all(ld.get(k) == val for k, val in match.items()):
            yield ld, v


def total(series, metric, **match):
    return sum(v for _, v in select(series, metric, **match))


def delta(a, b, metric, **match):
    return total(b, metric, **match) - total(a, metric, **match)


def grouped_delta(a, b, metric, keys, **match):
    acc = defaultdict(float)
    for ld, v in select(b, metric, **match):
        acc[tuple(ld.get(k, "") for k in keys)] += v
    for ld, v in select(a, metric, **match):
        acc[tuple(ld.get(k, "") for k in keys)] -= v
    return acc


def hist_quantile(a, b, metric, q, **match):
    buckets = defaultdict(float)
    for ld, v in select(b, metric + "_bucket", **match):
        buckets[ld["le"]] += v
    for ld, v in select(a, metric + "_bucket", **match):
        buckets[ld["le"]] -= v
    if not buckets:
        return None
    items = sorted(((math.inf if le == "+Inf" else float(le)), c) for le, c in buckets.items())
    count = items[-1][1]
    if count <= 0:
        return None
    target = q * count
    prev_le, prev_c = 0.0, 0.0
    for le, c in items:
        if c >= target:
            if le == math.inf:
                return prev_le
            span = c - prev_c
            frac = (target - prev_c) / span if span > 0 else 0
            return prev_le + (le - prev_le) * frac
        prev_le, prev_c = le, c
    return prev_le


def mean_of(a, b, metric, **match):
    c = delta(a, b, metric + "_count", **match)
    return delta(a, b, metric + "_sum", **match) / c if c > 0 else None


def window(snaps, t0, t1):
    inside = [s for s in snaps if t0 <= s["t"] <= t1]
    if len(inside) < 2:
        before = [s for s in snaps if s["t"] <= t0]
        after = [s for s in snaps if s["t"] >= t1]
        inside = ([before[-1]] if before else []) + inside + ([after[0]] if after else [])
    return inside


def cadvisor(snap, metric, container):
    return sum(v for ld, v in select(snap.get("cadvisor", {}), metric, container=container))


def analyse(snaps, t0, t1):
    w = window(snaps, t0, t1)
    if len(w) < 2:
        return None
    a, b = w[0], w[-1]
    secs = b["t"] - a["t"]
    ma, mb = a.get("manager", {}), b.get("manager", {})
    aa, ab = a.get("apiserver", {}), b.get("apiserver", {})
    ea, eb = a.get("etcd", {}), b.get("etcd", {})
    r = {"seconds": secs, "snapshots": len(w)}
    rate = lambda x: x / secs if secs else None

    r["reconciles_per_s"] = {c: rate(v) for (c,), v in grouped_delta(ma, mb, "controller_runtime_reconcile_total", ["controller"]).items() if v}
    r["reconcile_errors_per_s"] = {c: rate(v) for (c,), v in grouped_delta(ma, mb, "controller_runtime_reconcile_total", ["controller"], result="error").items() if v}
    r["vm_reconcile_time_s"] = {
        "mean": mean_of(ma, mb, "controller_runtime_reconcile_time_seconds", controller="virtualmachine"),
        "p50": hist_quantile(ma, mb, "controller_runtime_reconcile_time_seconds", 0.5, controller="virtualmachine"),
        "p99": hist_quantile(ma, mb, "controller_runtime_reconcile_time_seconds", 0.99, controller="virtualmachine"),
    }
    depths = [total(s.get("manager", {}), "workqueue_depth", name="virtualmachine") for s in w]
    r["vm_queue_depth"] = {"mean": sum(depths) / len(depths), "max": max(depths)}
    r["vm_queue_wait_s"] = {
        "mean": mean_of(ma, mb, "workqueue_queue_duration_seconds", name="virtualmachine"),
        "p99": hist_quantile(ma, mb, "workqueue_queue_duration_seconds", 0.99, name="virtualmachine"),
    }
    host_depths = [total(s.get("manager", {}), "workqueue_depth", name="host") for s in w]
    r["host_queue_depth"] = {"mean": sum(host_depths) / len(host_depths), "max": max(host_depths)}

    api = grouped_delta(ma, mb, "spike_manager_api_requests_total", ["verb", "resource", "subresource"])
    r["api_requests_per_s"] = {"/".join(k).rstrip("/"): rate(v) for k, v in sorted(api.items(), key=lambda kv: -kv[1]) if v > 0}
    r["api_total_per_s"] = rate(sum(api.values()))
    r["api_writes_per_s"] = rate(sum(v for k, v in api.items() if k[0] in ("POST", "PUT", "PATCH", "DELETE")))
    r["rate_limiter_wait_s"] = {
        "mean": mean_of(ma, mb, "spike_manager_client_rate_limiter_wait_seconds"),
        "p50": hist_quantile(ma, mb, "spike_manager_client_rate_limiter_wait_seconds", 0.5),
        "p99": hist_quantile(ma, mb, "spike_manager_client_rate_limiter_wait_seconds", 0.99),
    }
    r["api_latency_s"] = {
        "mean": mean_of(ma, mb, "spike_manager_api_request_duration_seconds"),
        "p99": hist_quantile(ma, mb, "spike_manager_api_request_duration_seconds", 0.99),
    }
    rpcs = grouped_delta(ma, mb, "virtrigaud_provider_rpc_requests_total", ["method"])
    r["provider_rpcs_per_s"] = {m: rate(v) for (m,), v in sorted(rpcs.items(), key=lambda kv: -kv[1]) if v > 0}
    r["provider_rpc_latency_s"] = {m: {"mean": mean_of(ma, mb, "virtrigaud_provider_rpc_latency_seconds", method=m),
                                       "p99": hist_quantile(ma, mb, "virtrigaud_provider_rpc_latency_seconds", 0.99, method=m)}
                                   for (m,), v in rpcs.items() if v > 0}
    r["placement_lock"] = {
        "decisions": delta(ma, mb, "spike_placement_lock_hold_seconds_count"),
        "busy": delta(ma, mb, "spike_placement_lock_wait_seconds_count", acquired="false"),
        "wait_mean_s": mean_of(ma, mb, "spike_placement_lock_wait_seconds"),
        "wait_p99_s": hist_quantile(ma, mb, "spike_placement_lock_wait_seconds", 0.99),
        "hold_mean_s": mean_of(ma, mb, "spike_placement_lock_hold_seconds"),
        "hold_p99_s": hist_quantile(ma, mb, "spike_placement_lock_hold_seconds", 0.99),
    }
    r["go"] = {
        "alloc_bytes_per_s": rate(delta(ma, mb, "go_memstats_alloc_bytes_total")),
        "gc_per_s": rate(delta(ma, mb, "go_gc_duration_seconds_count")),
        "gc_pause_s_per_s": rate(delta(ma, mb, "go_gc_duration_seconds_sum")),
        "heap_inuse_max_bytes": max(total(s.get("manager", {}), "go_memstats_heap_inuse_bytes") for s in w),
        "rss_max_bytes": max(total(s.get("manager", {}), "process_resident_memory_bytes") for s in w),
        "process_cpu_cores": rate(delta(ma, mb, "process_cpu_seconds_total")),
        "goroutines_max": max(total(s.get("manager", {}), "go_goroutines") for s in w),
    }
    periods = cadvisor(b, "container_cpu_cfs_periods_total", "manager") - cadvisor(a, "container_cpu_cfs_periods_total", "manager")
    throttled = cadvisor(b, "container_cpu_cfs_throttled_periods_total", "manager") - cadvisor(a, "container_cpu_cfs_throttled_periods_total", "manager")
    r["manager_container"] = {
        "cpu_cores": rate(cadvisor(b, "container_cpu_usage_seconds_total", "manager") - cadvisor(a, "container_cpu_usage_seconds_total", "manager")),
        "throttled_period_fraction": throttled / periods if periods else None,
        "throttled_s_per_s": rate(cadvisor(b, "container_cpu_cfs_throttled_seconds_total", "manager") - cadvisor(a, "container_cpu_cfs_throttled_seconds_total", "manager")),
        "working_set_max_bytes": max(cadvisor(s, "container_memory_working_set_bytes", "manager") for s in w),
    }
    r["provider_container_cpu_cores"] = rate(cadvisor(b, "container_cpu_usage_seconds_total", "provider") - cadvisor(a, "container_cpu_usage_seconds_total", "provider"))
    vm_updates_etcd = delta(aa, ab, "etcd_requests_total", resource="virtualmachines", operation="update")
    mgr_vm_status_puts = api.get(("PUT", "virtualmachines", "status"), 0.0)
    r["apiserver"] = {
        "requests_per_s": rate(delta(aa, ab, "apiserver_request_total")),
        "requests_by_verb_per_s": {v: rate(x) for (v,), x in grouped_delta(aa, ab, "apiserver_request_total", ["verb"]).items() if x},
        "cpu_cores": rate(delta(aa, ab, "process_cpu_seconds_total")),
        "rss_max_bytes": max(total(s.get("apiserver", {}), "process_resident_memory_bytes") for s in w),
        "etcd_vm_updates_per_s": rate(vm_updates_etcd),
        "manager_vm_status_puts_per_s": rate(mgr_vm_status_puts),
        "inflight_max": max(total(s.get("apiserver", {}), "apiserver_current_inflight_requests") for s in w),
        "flowcontrol_rejected": delta(aa, ab, "apiserver_flowcontrol_rejected_requests_total"),
        "flowcontrol_wait_mean_s": mean_of(aa, ab, "apiserver_flowcontrol_request_wait_duration_seconds"),
    }
    r["etcd"] = {
        "proposals_per_s": rate(delta(ea, eb, "etcd_server_proposals_committed_total")),
        "cpu_cores": rate(delta(ea, eb, "process_cpu_seconds_total")),
        "db_bytes": total(eb, "etcd_mvcc_db_total_size_in_bytes"),
        "wal_fsync_mean_s": mean_of(ea, eb, "etcd_disk_wal_fsync_duration_seconds"),
    }
    fakes = {}
    for k in b:
        if k.startswith("fake-"):
            fa, fb = a.get(k, {}), b[k]
            calls = grouped_delta(fa, fb, "spike_fake_rpc_total", ["method"])
            fakes[k] = {
                "rpcs_per_s": {m: rate(v) for (m,), v in calls.items() if v > 0},
                "duplicate_identity": total(fb, "spike_fake_duplicate_identity_total"),
                "vms": total(fb, "spike_fake_vms"),
                "slot_wait_create_mean_s": mean_of(fa, fb, "spike_fake_host_slot_wait_seconds", method="create"),
            }
    r["fake"] = fakes
    return r


def main():
    d = sys.argv[1]
    snaps = load_snapshots(d)
    m = markers(d)
    names = sys.argv[2:] or sorted({k[: -len("_start")] for k in m if k.endswith("_start") and k[: -len("_start")] + "_end" in m},
                                   key=lambda n: m[n + "_start"])
    out = {}
    for n in names:
        res = analyse(snaps, m[n + "_start"], m[n + "_end"])
        if res:
            out[n] = res
    with open(os.path.join(d, "analysis.json"), "w") as f:
        json.dump(out, f, indent=1, sort_keys=True)
    for n, r in out.items():
        print(f"== {n} ({r['seconds']} s)")
        print(f"  reconciles/s {json.dumps({k: round(v, 2) for k, v in r['reconciles_per_s'].items()})}")
        print(f"  api total/s {r['api_total_per_s']:.1f} writes/s {r['api_writes_per_s']:.1f} ratelimit wait mean/p99 "
              f"{r['rate_limiter_wait_s']['mean']} / {r['rate_limiter_wait_s']['p99']}")
        print(f"  rpcs/s {json.dumps({k: round(v, 2) for k, v in r['provider_rpcs_per_s'].items()})}")
        print(f"  cpu cores {r['manager_container']['cpu_cores']} throttled {r['manager_container']['throttled_period_fraction']} "
              f"ws {r['manager_container']['working_set_max_bytes']/2**20:.0f} MiB  queue depth max {r['vm_queue_depth']['max']}")
        print(f"  etcd VM updates/s {r['apiserver']['etcd_vm_updates_per_s']} vs manager status PUT/s {r['apiserver']['manager_vm_status_puts_per_s']}")


if __name__ == "__main__":
    main()
