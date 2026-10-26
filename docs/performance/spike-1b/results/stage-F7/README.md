# Stage F7: post-Create readiness bursts

Results behind [`docs/performance/f7-post-create-readiness.md`](../../../f7-post-create-readiness.md).
One kind cluster (`virtrigaud-spike1b`) at steady state: 1 clustered Provider,
1,000 Hosts, 10,000 seeded VMs, manager configuration A (500m CPU, 512 MiB,
QPS 20 / burst 40). The per-snapshot `metrics/` directories are kept locally
and not committed.

## Which manager image each run used

| Directory | Image | Run |
|---|---|---|
| `AF5-control-a/` | `spike1b.local/manager:f1f5` | 120 s steady window, 100-VM burst |
| `AF5-control-b/` | `spike1b.local/manager:f1f5` | 120 s steady window, 100-VM burst |
| `AF7/` | `spike1b.local/manager:f7` | 120 s steady window, 100-VM burst |
| `small-burst/f7/` | `spike1b.local/manager:f7` | 10-VM burst |
| `small-burst/f1f5/` | `spike1b.local/manager:f1f5` | 10-VM burst |

`f1f5` is the manager at `1f30f709` (F1–F5); `f7` is the same tree plus the F7
change. Both carry the unchanged Spike 1B experiment patch
(`hack/bench/spike1b/build-images.sh`). Each directory's `image.txt` (or
`NOTE.txt`) records the image read back from the Deployment after the switch.

## Correction: the first attempt never ran F7

The first attempt ([`../stage-F7.log`](../stage-F7.log)) switched images with
`helm upgrade` (the harness's `use_config`). In this cluster the chart's CRD
pre-upgrade hook (`virtrigaud-crd-upgrade-*` job) failed both times
(`UPGRADE FAILED: pre-upgrade hooks failed`); the harness carried on, and the
rollout restart kept the old image. Both runs of that attempt therefore used
`f1f5`. They were renamed from `AF7/` and `AF5/` to `AF5-control-a/` and
`AF5-control-b/` (each has a `NOTE.txt`) and serve as F1–F5 controls. The log
still shows the original labels: its `config AF7` is `AF5-control-a/`, its
`config AF5` is `AF5-control-b/`.

Every later run ([`../stage-F7-run2.log`](../stage-F7-run2.log) and
`small-burst/`) switched the image directly, which is configuration-equivalent
because the two values files differ only in the image tag:

```bash
kubectl --context kind-virtrigaud-spike1b -n virtrigaud-system set image deploy/virtrigaud-manager manager=spike1b.local/manager:<tag>
```

then waited for the rollout and for the VirtualMachine work queue to stay at 0
for three 10 s samples, and recorded the image. The hook failure was not
investigated further; it is a harness and chart-environment issue, not part of
F7.
