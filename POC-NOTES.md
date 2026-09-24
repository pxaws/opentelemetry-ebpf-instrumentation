# PoC notes: Service Connect peer.service.name

Branch `poc/sc-envoy-peer-name`, cut from `local-omni-us-east-1` at `8880a21`
(a local `git clone` of `setup-local-omni/obi-ping-demo`). The push URL is set
to `DISABLED`; no commit leaves this machine. (The rig's collector config, which since run 3
also exports to CloudWatch, lives in `ecs-investigation/experiments/sc-obi-poc/collector-config.yaml`,
not here.) Plan:
`ecs-investigation/docs/plans/2026-09-23-2305-feat-sc-envoy-peer-service-name-poc-plan.md`.

## How to build and test on this desktop

The desktop has no clang, and `proxy.golang.org` resolves to a DNS sinkhole.
Everything else the build needs is reachable, so:

| Step | Command (from `ecs-investigation/`) |
|---|---|
| BPF bindings (gitignored) | `make -C obi-sc-poc docker-generate` |
| Go modules | `GOPROXY=direct` on the host (about 2.5 min cold) |
| Go unit tests | `experiments/sc-obi-poc/dev-in-docker.sh go test -short ./pkg/...` — add `AS_ROOT=1` for tests that load BPF, and `DOCKER_EXTRA="--dns 8.8.8.8"` for DNS tests |
| BPF pure-policy tests | `experiments/sc-obi-poc/bpf-test-in-docker.sh` (Ubuntu + apt clang, as in CI) |
| Collector image | `experiments/sc-obi-poc/10-collector-image.sh [--push]` — compiles on the host, then a thin alpine image |

## Baseline at 8880a21 (before any PoC change)

Host kernel 5.10 (amzn2int). Go 1.26.3 on the host, 1.25.11 in the generator image.

- `bpf/tests`: 18/18 pass.
- `go test -short ./pkg/internal/ebpf/tpinjector/... ./pkg/ebpf/common/... ./pkg/transform/...`,
  in the container as root with public DNS: all pass except
  `TestTracer_Constants` (every subtest: "bundle count must match, has 1").
  It depends on the kernel FIONREAD probe; on this 5.10 kernel the
  compensation program does not load, so only one bundle comes back.
  This failure is environmental and pre-existing. The gate for later
  commits is "no new failures".
- On the host itself, three more tests fail for environment reasons only:
  `TestCWDForPID` (`/home` is a symlink to `/local/home`), and
  `TestResolver` + `TestRDNSMissFallsThroughToDNS` (the corp resolver answers
  for names that must not resolve).
- Collector image `sc-obi-poc-collector:8880a21` builds locally.

## How the PoC is wired

- **Callee stamp (U3).** `tpinjector.AllowPID` offers each discovered app's name to a
  borrow loop (`proxy_borrow.go`, 5 s reconcile). For each non-host netns with exactly one
  distinct app name, every Envoy pid there gets that name in `proxy_svc_name_by_pid`
  (tracked by pid + start time). In sk_msg, `svc_name_source` (`proxy_stamp_policy.h`)
  uses it only when the writer has no own name and the peer is not loopback or 0.0.0.0.
- **Caller learns per backend (U4).** `parse_svc_name_hdr_opt` also writes
  `svc_name_by_peer[{remote_ip4 (network order), bpf_ntohl(remote_port)}]`, loopback
  skipped. `Tracer.PeerNames()` snapshots it.
- **Join (U5).** `scproxy.Joiner` (10 s tick) reads each sidecar's admin socket
  (GET `/config_dump`, GET `/clusters?format=json`) and applies the agreement rule.
  `transform.SCPeerNameProvider` (node `ServiceConnectPeerName`, between DockerDecorator
  and NameResolution) sets `PeerServiceName` on client spans to 127.255.0.0/16.
- **Holder threading (U5).** `global.ContextInfo.SCPeerNames` is created in
  `instrumenter.BuildCommonContextInfo` (before `appolly.Build`), passed by
  `finder.go` into `traceAttacher.SCPeerNames`, and filled by
  `traceAttacher.publishPeerNames()` from `dropUnloadedTracers`, which runs right after
  `ProcessTracer.Init` loaded the BPF objects. No tpinjector hook and no global. An empty
  holder means "no names learned".
- **Config.** `scproxy: {enabled, interval, block_list_file, proc_root}` on `obi.Config`,
  default off. The block-list file (one netns inode per line) is re-read every tick.

## After U5

All gates as in the baseline: BPF tests 20/20; Go failures are only the four
environmental ones listed above. The tpinjector programs load in the 5.10 verifier
(checked by the U3 worker as root in a privileged container, inject_flags=3).
