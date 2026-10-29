#!/usr/bin/env python3
"""Spike 1B: render the report's tables from results/ (analysis.json and
*-summary.json files). Usage: tables.py <results-dir> > tables.md"""

import glob
import json
import os
import sys


def f(v, digits=1, scale=1.0):
    if v is None:
        return "—"
    return f"{v * scale:.{digits}f}"


def steady_rows(res):
    print("| Stage | Config | VM reconciles/s | Host reconciles/s | API req/s | API writes/s | Rate-limit wait p99 (s) | "
          "VM queue depth mean / max | Describe/s | Validate/s | Manager CPU (cores) | CFS throttled periods | "
          "Working set max (MiB) | Alloc (MB/s) | etcd VM updates/s vs status PUT/s |")
    print("|" + "---|" * 15)
    for stage in sorted(glob.glob(os.path.join(res, "stage-*"))):
        for cdir in sorted(glob.glob(os.path.join(stage, "*"))):
            p = os.path.join(cdir, "analysis.json")
            if not os.path.exists(p):
                continue
            a = json.load(open(p)).get("steady")
            if not a:
                continue
            rc = a["reconciles_per_s"]
            api = a["apiserver"]
            print(f"| {os.path.basename(stage)[6:]} | {os.path.basename(cdir)} | {f(rc.get('virtualmachine'))} | {f(rc.get('host'), 2)} | "
                  f"{f(a['api_total_per_s'])} | {f(a['api_writes_per_s'])} | {f(a['rate_limiter_wait_s']['p99'], 2)} | "
                  f"{f(a['vm_queue_depth']['mean'], 0)} / {f(a['vm_queue_depth']['max'], 0)} | "
                  f"{f(a['provider_rpcs_per_s'].get('Describe'))} | {f(a['provider_rpcs_per_s'].get('Validate'))} | "
                  f"{f(a['manager_container']['cpu_cores'], 2)} | {f(a['manager_container']['throttled_period_fraction'], 2)} | "
                  f"{f(a['manager_container']['working_set_max_bytes'], 0, 1 / 2**20)} | {f(a['go']['alloc_bytes_per_s'], 1, 1e-6)} | "
                  f"{f(api['etcd_vm_updates_per_s'])} vs {f(api['manager_vm_status_puts_per_s'])} |")


def burst_rows(res):
    print("| Stage | Config | Burst | Ready | First reconcile p50 (s) | pendingHost p50 / p99 (s) | status.id p50 (s) | "
          "Ready p50 / p90 / p99 (s) | Ready/s | Delete→gone p50 (s) | Lock hold mean / p99 (ms) | Lock wait p99 (ms) | Lock busy |")
    print("|" + "---|" * 13)
    for stage in sorted(glob.glob(os.path.join(res, "stage-*"))):
        for cdir in sorted(glob.glob(os.path.join(stage, "*"))):
            an = {}
            p = os.path.join(cdir, "analysis.json")
            if os.path.exists(p):
                an = json.load(open(p))
            for sp in sorted(glob.glob(os.path.join(cdir, "b*-summary.json")), key=lambda x: json.load(open(x))["submitted"]):
                s = json.load(open(sp))
                n = s["submitted"]
                lat = s["latencyMs"]
                g = lambda st, q: (lat.get(st, {}).get(q) / 1000) if lat.get(st, {}).get(q) is not None else None
                lock = an.get(f"burst_{n}", {}).get("placement_lock", {})
                print(f"| {os.path.basename(stage)[6:]} | {os.path.basename(cdir)} | {n} | {s['reached'].get('ready', 0)} | "
                      f"{f(g('reconciled', 'p50'))} | {f(g('pending', 'p50'))} / {f(g('pending', 'p99'))} | {f(g('bound', 'p50'))} | "
                      f"{f(g('ready', 'p50'))} / {f(g('ready', 'p90'))} / {f(g('ready', 'p99'))} | {f(s['throughputPerSec'].get('ready'), 2)} | "
                      f"{f(g('gone', 'p50'))} | {f(lock.get('hold_mean_s'), 0, 1000)} / {f(lock.get('hold_p99_s'), 0, 1000)} | "
                      f"{f(lock.get('wait_p99_s'), 0, 1000)} | {f(lock.get('busy'), 0)} |")


def api_cost(res):
    print("| Stage | Config | Burst | VM API requests per created VM (manager) | of which writes | Describe+Validate RPCs per VM |")
    print("|---|---|---|---|---|---|")
    for stage in sorted(glob.glob(os.path.join(res, "stage-*"))):
        for cdir in sorted(glob.glob(os.path.join(stage, "*"))):
            p = os.path.join(cdir, "analysis.json")
            if not os.path.exists(p):
                continue
            an = json.load(open(p))
            st = an.get("steady")
            for k, a in an.items():
                if not k.startswith("burst_") or not st:
                    continue
                n = int(k.split("_")[1])
                secs = a["seconds"]
                extra = (a["api_total_per_s"] - st["api_total_per_s"]) * secs
                extra_w = (a["api_writes_per_s"] - st["api_writes_per_s"]) * secs
                rpc = sum(a["provider_rpcs_per_s"].get(m, 0) - st["provider_rpcs_per_s"].get(m, 0) for m in ("Describe", "Validate")) * secs
                print(f"| {os.path.basename(stage)[6:]} | {os.path.basename(cdir)} | {n} | {f(extra / n)} | {f(extra_w / n)} | {f(rpc / n)} |")


if __name__ == "__main__":
    res = sys.argv[1]
    print("### Steady state\n")
    steady_rows(res)
    print("\n### Bursts\n")
    burst_rows(res)
    print("\n### API cost of a burst above steady state (estimate: burst window minus steady rate)\n")
    api_cost(res)
