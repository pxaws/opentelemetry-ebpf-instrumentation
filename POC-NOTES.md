# PoC notes: Service Connect peer.service.name

Branch `poc/sc-envoy-peer-name`, cut from `local-omni-us-east-1` at `8880a21`
(a local `git clone` of `setup-local-omni/obi-ping-demo`). The push URL is set
to `DISABLED`; nothing leaves this machine. Plan:
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
