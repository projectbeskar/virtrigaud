# Agent journal durability runs

Scripts that exercise the per-host agent journal (`internal/agent/journal`) on
real block devices. They are test tooling only: they run as root **inside a
disposable test VM**, never on a shared host. The driver is
`test/agent-lab/cmd/journal-torture`.

## Driver

Build it statically and copy it into the VM:

```sh
CGO_ENABLED=0 go build -trimpath -o journal-torture ./test/agent-lab/cmd/journal-torture
```

- `journal-torture run -dir D -ack SINK [-n N] [-dm-mark DEV]` commits one
  operation per transaction. An operation is acknowledged only after
  `Update` returns: optionally as a dm-log-writes mark `ack:<i>`, then as a
  line `ACK <i> <seq>` written to SINK and made durable (fsync for a file,
  `tcdrain` for a serial line). SINK must be outside the volume under test.
- `journal-torture verify -dir D (-ack FILE | -acked N) [-probe] [-reopen]`
  reopens the store and prints one JSON line. Verdicts: `ok` (every
  acknowledged operation present and intact), `detected` (the store is
  quarantined, with its lost reason), `undetected_loss` (the store opens
  usable but an acknowledged operation is missing or altered: a failure),
  `availability` (usable store, probe commit failed), `anomaly`, `error`.
  `tail` reports the anchor tail case seen at open (`equal`,
  `store_ahead_1`, `torn_tail`).
- `journal-torture lwmarks -log DEV` lists the marks of a dm-log-writes log
  with their entry index.

Scripts default to `JT=/opt/jt/journal-torture`; `replay-torn.sh` also needs
`REPLAY_LOG` (default `/opt/lw/replay-log`), the `replay-log` tool from the
upstream xfstests `src/log-writes` directory, built from a pinned release.

## Runs

| Script | What it does |
|---|---|
| `replay-torn.sh FSTYPE OPS WORKDIR` | Records format + workload through dm-log-writes, then replays the write log entry by entry and verifies a throw-away snapshot after every entry (`replay-check.sh`). Every entry is a possible power-loss point. |
| `flakey-modes.sh MODE CYCLES WORKDIR` | dm-flakey `drop_writes`, `error_writes`, `corrupt_bio_byte` (writes) or `random_write_corrupt` while the workload runs, then a crash (all outstanding writes dropped), healthy remount and verify. |
| `eio.sh TARGET CYCLES WORKDIR` | Real EIO from dm-error over the store file's blocks (`store`), over the anchor log's blocks (`anchor`), or dm-flakey `error_writes` over the whole volume (`device`). Verifies with the fault in place, then after healing, then from a fresh process. |
| `powercut-guest.sh` / `powercut-host.sh` | Power cut: the VM's state disk is attached with `cache=none`; the host boots the VM, verifies against the acknowledgements received on a serial line, runs the workload for a random time and SIGKILLs the QEMU process. |

Results are JSON lines; `common.sh` has `verdict_counts`.

## Reading the results

- `undetected_loss` under `replay-torn.sh`, `eio.sh` or the power-cut run is a
  journal defect.
- Under `flakey-modes.sh drop_writes` and the corrupt modes the device
  acknowledges writes it did not store, or stores other bytes. The journal
  documents that storage which acknowledges a flush it did not perform
  defeats its durability guarantee, and that a damaged data page that still
  decodes is not detected; those runs measure how much is still detected.
