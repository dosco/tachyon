# Development

tachyon is a Linux-only proxy. The performance thesis rests on Linux kernel
primitives (io_uring, `SO_REUSEPORT` with BPF flow steering, kTLS) that
have no macOS or Windows equivalent. But development happens anywhere.

The standing arrangement:

- **Edit on macOS** (or wherever your laptop is). The pure-Go subtrees
  (`buf/`, `http1/`, `http2/`, `tlsutil/` proper, `metrics/`, `internal/*`,
  `cmd/*`) compile and most of them test cleanly on macOS.
- **Build + run on GCE Linux.** Anything touching io_uring, `SO_REUSEPORT`
  semantics, `sched_setaffinity`, kTLS, or benchmarks runs on a GCE spot
  instance. Same box will run the published benchmark numbers.

## The GCE dev box

One spot instance, `tachyon-dev`, `c4-standard-16` in `us-central1-a`:

- 16 vCPU (8 physical cores) Intel Emerald Rapids
- 60 GB RAM
- Ubuntu 24.04 LTS, kernel ≥6.8 (every io_uring op we need is present)
- Spot pricing ≈ $0.25/hr on-demand-equivalent

Spot means Google can preempt with 30 s notice. On preemption the VM is
STOPPED (not deleted); bring it back with `bench/gcloud-up.sh start`.

## One-time setup

```bash
# 1. gcloud must be authenticated and point at the right project.
gcloud auth login
gcloud config set project personal-493610
gcloud config set compute/zone us-central1-a

# 2. Create the VM (takes ~30s).
bench/gcloud-up.sh create

# 3. Provision it (Go, wrk2, bombardier, h2load, perf, bpftrace, ...).
gcloud compute scp --zone=us-central1-a bench/provision.sh tachyon-dev:/tmp/
gcloud compute ssh  --zone=us-central1-a tachyon-dev --command='bash /tmp/provision.sh'
```

## Daily loop

```bash
# sync local tree to VM (tar + scp, ~40 KiB)
bench/gcloud-sync.sh

# sync + go build ./...
bench/gcloud-sync.sh --build

# sync + build + Phase 0 smoke test (origin + proxy + 600 curls)
bench/gcloud-sync.sh --smoke

# drop into the VM
bench/gcloud-up.sh ssh

# cost-hygiene when you step away
bench/gcloud-up.sh stop
```

Inside the VM the repo is at `~/tachyon`. `cd ~/tachyon && go test ./...`
behaves exactly like on macOS.

## Build-tag conventions

Files that use Linux-only syscalls must be guarded:

```go
//go:build linux
```

As of Phase 0 only `internal/runtime/affinity.go` and
`internal/runtime/fork.go` need the guard; they each have a `*_other.go`
sibling with `//go:build !linux` and a no-op / fallback body. This keeps
`go build ./...` green on macOS.

When Phase 2 lands, every `iouring/**/*.go` that does real work will
need `//go:build linux`. The `doc.go` stubs stay portable so
`go doc tachyon/iouring` works cross-platform.

## When to ssh vs sync

- **Tight loop, breakpoint-style debugging:** ssh in, edit with `vim` or
  `nano`, build in place. Faster feedback, loses your editor niceties.
- **Larger edits / refactors / touching many files:** edit on macOS,
  `bench/gcloud-sync.sh --build` every few minutes.

The sync script is ~40 KiB each way. It's cheap; re-run it freely.

## Benchmarks

Bench runs always happen on the VM — laptop virtualization and thermal
throttling make laptop numbers meaningless. The harness expects the
proxy + origin + load generator all on the VM (colocated), which gets us
the best-case number; cross-network runs come later.

```bash
# on the VM:
cd ~/tachyon
./bench/run.sh tachyon           # single proxy
./bench/compare.sh               # nginx/pingora/envoy/traefik/tachyon
```

Results land under `results/<date>/<proxy>/<scenario>.txt`.

## Why GCE spot over Lima / Docker

We looked at Lima (macOS-native Linux VM) and Docker Desktop. The
deciding factors:

1. **Kernel choice.** io_uring features cluster on kernel version.
   Whatever Apple's virtualization framework hands us is fine, but GCE
   gives us a kernel matching what the authoritative bench numbers will
   use.
2. **Bench isolation.** On a laptop every other process distorts latency
   tails. On a dedicated VM only our proxy runs.
3. **One environment, not two.** The dev box and the bench box are the
   same box. Numbers from `--smoke` are directionally meaningful.
4. **Preemption is fine.** Stop the box when you're done for the day
   (`bench/gcloud-up.sh stop`). Spot pricing makes "leave it running
   over lunch" economical.
