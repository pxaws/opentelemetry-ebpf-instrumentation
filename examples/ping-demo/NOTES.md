# ping demo — usage guide

A 4-service HTTP app for exercising OBI's RED metrics, distributed traces, and the
experimental TCP-option `peer.service.name` propagation — end to end into Amazon
CloudWatch (metrics) and AWS X-Ray (traces).

```
checkout ──POST /charges──▶ payment ──POST /audit──▶ audit   (leaf)
   └───────POST /reservations──▶ inventory                   (leaf)
```

`app/server.py` is one Flask+gunicorn codebase; each instance's behaviour is set by
env vars, so the same image runs every role.

| env | meaning |
|---|---|
| `ROLE` | `checkout` / `payment` / `inventory` / `audit` — served route + downstream calls |
| `PROFILE` | `healthy` / `degraded` / `erroring` / `faulty` — status-code mix + latency of this service's own responses |
| `PAYMENT_URLS` / `INVENTORY_URLS` / `AUDIT_URLS` | comma-separated downstream base URLs (a DNS name is fine — see Discovery) |
| `USE_TLS` | `1` serves HTTPS with a self-signed cert (demo only) |
| `PORT` | listen port (default 8000) |
| `CPU_SHARE` | fraction of each response's target duration spent on real CPU work instead of sleeping (default `0`). Read at import, so every gunicorn worker sees it — unlike `POST /fault` below. At `0` the app behaves exactly as before. Above `0` an uncontended host's total duration is unchanged, but the compute portion now stretches when something else on the host wants the CPU, which is what makes a host-CPU fault visible in this service's own latency. `/healthz` reports the effective (parsed and clamped) value plus the answering worker's pid |

Runtime controls: `POST /fault?profile=<name>` flips `PROFILE` in **the one gunicorn worker that
answers it** — not instance-wide. There are two workers with no `--preload`, so the endpoint changes
roughly half the traffic, and `GET /healthz` reports whichever worker answered, so the confirming read
agrees about half the time. `GET /healthz` also reports the effective `CPU_SHARE` and the answering
worker's `pid`; sample it until both pids have answered before believing either value.

---

## 1. Images

**App** — built from this folder, no registry needed:
```bash
docker build -t ping-app app/
```

**Collector** — the OBI-as-receiver collector, built from `examples/otel-collector`
(ocb) and published multi-arch to GHCR by the fork's `build-artifacts.yml` workflow.
It bundles: obi receiver, otlp_http + sigv4auth (CloudWatch/X-Ray), cumulativetodelta,
transformprocessor (+ spanmetrics/filter, unused by default).

**Pin by digest, never by tag.** Route emission — `http.route` on both the server and the client
metric — is an unexplained property of one specific build: the running rig's collector has no
`routes:` block in its configuration and emits routes anyway. The whole per-operation roster in the
Omni UI depends on it, so a moving reference could collapse that roster to bare HTTP methods with no
error anywhere. `latest` moves by definition and a short-sha tag is mutable in GHCR; only the digest
cannot change under the rig.

```bash
# the pinned build (verified 2026-09-09: identical to tag f5027fd and to latest; built 03:07 UTC)
docker pull ghcr.io/wangzlei/obi-collector@sha256:cc750830356b1361b0762117287a41395ad018c2411ecf4da644906ec4fc9016
# rebuild after an OBI/builder-config change -- then re-verify route emission and re-pin the new digest
gh workflow run "Build artifacts (obi + collector)" \
  --repo wangzlei/opentelemetry-ebpf-instrumentation --ref demo/ping
```

Two things the image does NOT contain, both worth knowing before editing the config: `memory_limiter`
(neither processor nor extension — see `setup-local-omni/docs/issues.md` GAP 17) and any health-check
extension. The collector's own Prometheus telemetry endpoint on `:8888` is its only health signal.

`collector-config.yaml` (this folder) is mounted at `/etc/otelcol/config.yaml`. Validate an edit
offline, against the pinned image, before pushing it to eight hosts:

```bash
docker run --rm --entrypoint /otelcol \
  -e AWS_REGION=us-east-1 -e OMNI_BRIDGE_ENDPOINT=http://<bridge-private-ip>:4318 \
  -v "$PWD/collector-config.yaml:/cfg.yaml:ro" \
  ghcr.io/wangzlei/obi-collector@sha256:cc750830356b1361b0762117287a41395ad018c2411ecf4da644906ec4fc9016 \
  validate --config /cfg.yaml
# `print-config` instead of `validate` prints the RESOLVED config, which is how to confirm the two
# ${env:...} substitutions actually took.
```

**Two environment variables are required** by the config and are set in the run commands, never in the
file: `AWS_REGION` (both sigv4 signers and both AWS endpoints read it — the file is shared with the
us-west-2 rig whose redeploy is "write it and restart", so a hardcoded region puts cross-region
publication one command away) and `OMNI_BRIDGE_ENDPOINT` (the bridge's OTLP receiver, e.g.
`http://10.1.2.3:4318`; `otlp_http` appends `/v1/metrics`).

---

## 2. Run locally (docker-compose, single host)

Everything on one Docker host; exporters use your `~/.aws` (profile via `AWS_PROFILE`).
```bash
docker build -t ping-app app/
AWS_PROFILE=<profile> docker compose up -d
docker logs -f ping-collector      # watch export
```
`docker-compose.yaml` runs the four services + a traffic generator + the collector.

---

## 3. Run on EC2 (one service per host)

This is the deployment used for the reference setup: N instances, each running its
own collector + one app container, discovery via Route 53. There is no cross-host
orchestrator — each host is provisioned individually over SSM.

**Region for this deployment: `us-east-1`, account `868155213681`.** The original
reference rig runs in us-west-2 and is inventoried in `HANDOFF.md`; none of its resource
ids are valid here, because a VPC, subnet, security group and Route 53 zone are all
region-scoped. Only the AMI has a us-east-1 equivalent:

| resource | us-east-1 value |
|---|---|
| AMI (AL2023 x86, kernel 6.1) | `ami-025b6f0b1ac2ef9f7` — or resolve fresh from SSM parameter `/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-6.1-x86_64` |
| VPC | **create** |
| subnet | **create** |
| security group | **create** — intra `:8000` plus egress `:443` |
| instance profile | **create** — SSM + `cloudwatch:PutMetricData` + X-Ray put actions |
| Route 53 private zone `ping.internal` | **create**, associated with the new VPC |

Substitute the created ids into the commands below. Resolve the AMI rather than pasting
the id above if this file is more than a few weeks old — AL2023 republishes often.

### 3a. Launch instances (one per role; scale payment/audit)
```bash
aws ec2 run-instances --region us-east-1 \
  --image-id ami-025b6f0b1ac2ef9f7 --instance-type t3.small --count 3 \
  --subnet-id <new-subnet-id> --security-group-ids <new-sg-id> \
  --iam-instance-profile Arn=<obs-instance-profile-arn> \
  --metadata-options HttpTokens=required,HttpEndpoint=enabled \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=ping-payment},{Key=ping-role,Value=payment},{Key=purpose,Value=ping-demo}]'
```
Repeat per role (`checkout` ×1, `payment` ×3, `inventory` ×1, `audit` ×3).

### 3b. Route 53 records (method-2 discovery)
Multivalue A records, one value per instance IP, low TTL:
```bash
aws route53 change-resource-record-sets --hosted-zone-id <new-zone-id> \
  --change-batch '{"Changes":[{"Action":"UPSERT","ResourceRecordSet":{
    "Name":"payment.ping.internal","Type":"A","TTL":10,
    "ResourceRecords":[{"Value":"<ip1>"},{"Value":"<ip2>"},{"Value":"<ip3>"}]}}]}'
# repeat for audit (3), inventory (1), checkout (1)
```

### 3c. Provision each host (SSM `AWS-RunShellScript`)
Per host: install docker, drop the app files + `collector-config.yaml` under
`/opt/ping`, then run the collector (host-net, privileged) and one app container
(host-net) with the role's env. Downstream URLs are the DNS names above, e.g.:
- checkout: `PAYMENT_URLS=http://payment.ping.internal:8000 INVENTORY_URLS=http://inventory.ping.internal:8000`
- payment:  `AUDIT_URLS=http://audit.ping.internal:8000`

```bash
# The collector. Pinned by digest (section 1) and memory-capped: an OOM-killed collector drops batches,
# which reads as the agent limitation this rig exists to measure, so the cap makes an OOM a measured
# fact. This build has no memory_limiter processor, so the container cap is the only bound (GAP 17).
COLLECTOR=ghcr.io/wangzlei/obi-collector@sha256:cc750830356b1361b0762117287a41395ad018c2411ecf4da644906ec4fc9016
docker run -d --name ping-collector --restart unless-stopped --privileged --pid host \
  --network host --memory 512m \
  -e AWS_REGION=us-east-1 -e OMNI_BRIDGE_ENDPOINT=http://<bridge-private-ip>:4318 \
  -v /sys/fs/bpf:/sys/fs/bpf -v /sys:/sys -v /proc:/proc:ro \
  -v /opt/ping/collector-config.yaml:/etc/otelcol/config.yaml:ro \
  "$COLLECTOR" --config /etc/otelcol/config.yaml

docker run -d --name ping-app-svc --restart unless-stopped --network host --memory 256m \
  -e OTEL_SERVICE_NAME=payment-service -e ROLE=payment -e PROFILE=healthy \
  -e AUDIT_URLS=http://audit.ping.internal:8000 ping-app
```

`OMNI_BRIDGE_ENDPOINT` must be the bridge's real private address. A wrong one fails **quietly** by
design — the bridge exporter has retries off and a bounded queue, so it drops rather than backs up — so
confirm the bridge topic's message count rises after deploying, not just that the container is up.
Access hosts with `aws ssm start-session --target <instance-id>` (no SSH/public IP).

---

## 4. Service discovery / traffic fan-out

Downstream URLs are DNS names backed by Route 53 multivalue A records. The app
resolves the name to all A records and picks one per request (`server.py::_targets`),
sending `Host: <name>` — so traffic spreads evenly across instances while the client
`server.address` stays the clean DNS name and `peer.service.name` still resolves.
(Plain DNS + a pooled HTTP client would pin every request to one instance.)

---

## 5. Telemetry produced

RED = OBI native application metrics:
- `http.server.request.duration` — node RED, per service, split per instance by the
  `@resource.ec2.instance.id` / `service.instance.id` resource attributes.
- `http.client.request.duration` — edge RED; carries `server.address` (DNS name) **and**
  `peer.service.name` (downstream service's own name, from the kind-26 TCP option).

`collector-config.yaml` adds AWS-flavoured resource attributes to every span/metric via
a `transform` processor: `ec2.instance.id`, `aws.account.id`, `aws.region`, `host.ip`
(alongside OBI's native `host.id` / `cloud.*`).

---

## 6. Fault injection

- App latency / errors: `curl -XPOST 'http://<host>:8000/fault?profile=degraded'` on one
  instance (per-instance, live).
- Network latency (kernel qdisc): on one host over SSM —
  `dnf install -y iproute-tc; tc qdisc add dev ens5 root netem delay 400ms`
  (remove: `tc qdisc del dev ens5 root`). NOTE: netem sits below OBI's socket-level
  measurement, so the affected server's OWN duration stays normal; the latency shows on
  the caller's client edge (T_client) and propagates up — the client-vs-server gap is the
  network-vs-code signal. Application RED cannot name which instance is slow (client edge
  is aggregated by peer.service.name); enable OBI's `network`/TCP-RTT metrics for that.

---

## 7. Querying

CloudWatch OTLP metrics are a Prometheus-compatible store — use PromQL, not `list-metrics`:
- SigV4 POST `https://monitoring.<region>.amazonaws.com/api/v1/query`, body `query=<promql>`,
  signing service `monitoring`. Labels are prefixed (`@resource.service.name`,
  `@resource.ec2.instance.id`, …); a metric name is required, e.g.
  `{__name__="http.server.request.duration"}`.
- p99: `histogram_quantile(0.99, sum_over_time({__name__="http.server.request.duration","@resource.service.name"="payment-service"}[5m]))`
- count: `histogram_count(sum_over_time({__name__="http.server.request.duration","@resource.service.name"="payment-service"}[5m]))`

Traces: X-Ray API (`get-trace-summaries` + `batch-get-traces`, max 5 ids/call).
`peer.service.name` is on CLIENT spans.

---

## 8. Customising metric dimensions

- Resource attributes: add/rename in the `transform` processor (config only, no rebuild).
- New span-derived attributes on the native RED metrics: an OBI change (attribute name +
  span getter + the metric's attribute group) — see how `peer.service.name` was added.
- Fully arbitrary dimensions from any span/resource attribute: switch RED to the
  `spanmetricsconnector` (already compiled into the image; wire it in `collector-config.yaml`).
