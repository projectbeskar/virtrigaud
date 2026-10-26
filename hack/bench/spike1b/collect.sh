#!/usr/bin/env bash
# Spike 1B metrics collector. Every INTERVAL seconds it writes one snapshot
# directory under OUT/<unix-seconds>/ with gzipped Prometheus text from:
#   manager.prom    the manager's /metrics (experiment build; via the API pod proxy)
#   fake-<p>.prom   each fake provider's admin /metrics
#   apiserver.prom  kube-apiserver /metrics, filtered to request, etcd, storage and process series
#   etcd.prom       etcd /metrics (127.0.0.1:2381 inside the control-plane node), filtered
#   cadvisor.prom   the worker kubelet's cAdvisor, manager and provider containers only
# Runs until killed.
set -uo pipefail
CTX="${CTX:-kind-virtrigaud-spike1b}"
OUT="${OUT:?OUT is required}"
INTERVAL="${INTERVAL:-15}"
CP_NODE="${CP_NODE:-virtrigaud-spike1b-control-plane}"
WORKER="${WORKER:-virtrigaud-spike1b-worker}"
K="kubectl --context $CTX"
mkdir -p "$OUT"

APISERVER_RE='^(apiserver_request_total|apiserver_request_duration_seconds_(sum|count)|apiserver_current_inflight_requests|apiserver_storage_objects|apiserver_flowcontrol_(current_inqueue|request_wait_duration_seconds_(sum|count)|rejected_requests_total)|etcd_requests_total|etcd_request_duration_seconds_(sum|count)|process_cpu_seconds_total|process_resident_memory_bytes|apiserver_watch_events_total)'
ETCD_RE='^(etcd_mvcc_db_total_size_in_bytes|etcd_mvcc_db_total_size_in_use_in_bytes|etcd_server_proposals_committed_total|etcd_disk_wal_fsync_duration_seconds_(sum|count)|etcd_disk_backend_commit_duration_seconds_(sum|count)|etcd_mvcc_put_total|etcd_mvcc_range_total|etcd_mvcc_delete_total|process_cpu_seconds_total|process_resident_memory_bytes)'
CADVISOR_RE='^container_(cpu_usage_seconds_total|cpu_cfs_periods_total|cpu_cfs_throttled_periods_total|cpu_cfs_throttled_seconds_total|memory_working_set_bytes|memory_rss)\{.*container="(manager|provider)"'

pod_of() { $K -n "$1" get pod -l "$2" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null; }

while true; do
	ts=$(date +%s)
	d="$OUT/$ts"
	mkdir -p "$d"
	mgr=$(pod_of virtrigaud-system app.kubernetes.io/name=virtrigaud)
	if [[ -n "$mgr" ]]; then
		$K get --raw "/api/v1/namespaces/virtrigaud-system/pods/$mgr:8080/proxy/metrics" 2>/dev/null | gzip >"$d/manager.prom.gz" &
	fi
	for p in $($K -n spike-infra get provider -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
		fp=$(pod_of spike-infra "app.kubernetes.io/instance=$p")
		[[ -n "$fp" ]] && $K get --raw "/api/v1/namespaces/spike-infra/pods/$fp:8090/proxy/metrics" 2>/dev/null | gzip >"$d/fake-$p.prom.gz" &
	done
	$K get --raw /metrics 2>/dev/null | grep -E "$APISERVER_RE" | gzip >"$d/apiserver.prom.gz" &
	docker exec "$CP_NODE" curl -s http://127.0.0.1:2381/metrics 2>/dev/null | grep -E "$ETCD_RE" | gzip >"$d/etcd.prom.gz" &
	$K get --raw "/api/v1/nodes/$WORKER/proxy/metrics/cadvisor" 2>/dev/null | grep -E "$CADVISOR_RE" | gzip >"$d/cadvisor.prom.gz" &
	wait
	sleep "$INTERVAL"
done
