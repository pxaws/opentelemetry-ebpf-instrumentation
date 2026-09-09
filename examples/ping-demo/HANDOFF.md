# ping demo — live deployment handoff

> **This file inventories the ORIGINAL us-west-2 rig, and its region is deliberately
> left as `us-west-2`.** Every id below is region-scoped, so rewriting the region here
> would name resources that do not exist. The local Omni stack work targets a separate
> **us-east-1** deployment — see `NOTES.md` §3 for the resources it has to create.

The reference ping demo is running now. This is the inventory of live AWS resources
so you can take it over. Account `868155213681`, region `us-west-2`. Access is via
SSM (no SSH / public IPs) — use your own credentials for this account.

See `NOTES.md` for how the app/collector work and how to redeploy from scratch.

## Instances (tag `purpose=ping-demo`)

| role | instance id | private IP |
|---|---|---|
| checkout | i-07334e30db77c8da8 | 10.0.6.247 |
| inventory | i-0b02004536f14e479 | 10.0.6.157 |
| payment | i-0fb89cc43f46cd813 | 10.0.6.136 |
| payment | i-0e267fcea43f68202 | 10.0.6.246 |
| payment | i-07dfe2639ef9e9d5c | 10.0.6.164 |
| audit | i-0fa62fab8cd729d0c | 10.0.6.169  ← **netem fault active** |
| audit | i-0a8e9dbc65cef0aaf | 10.0.6.38 |
| audit | i-048113234085d5e02 | 10.0.6.172 |

List anytime:
```bash
aws ec2 describe-instances --region us-west-2 \
  --filters Name=tag:purpose,Values=ping-demo Name=instance-state-name,Values=running \
  --query 'Reservations[].Instances[].{id:InstanceId,role:Tags[?Key==`ping-role`]|[0].Value,ip:PrivateIpAddress}' --output table
```

Each host runs two containers: `ping-collector` (OBI-as-receiver collector) and
`ping-app-svc` (the service). Traffic generator (`ping-traffic`) runs on the checkout host.

## Networking / discovery

- VPC `vpc-017c34776120b35bc`, subnet `subnet-002be9598dad093c4`, SG `sg-0f6caf64203f751e2`.
- Route 53 private zone `ping.internal` = `Z097757611V01PJRE00N6`:
  `payment.ping.internal` / `audit.ping.internal` → 3 A records each (multivalue),
  `inventory.ping.internal` / `checkout.ping.internal` → 1 each.
  ```bash
  aws route53 list-resource-record-sets --profile <you> --hosted-zone-id Z097757611V01PJRE00N6
  ```

## Collector image

`ghcr.io/wangzlei/obi-collector` (public, multi-arch). Deployed tag = the demo/ping OBI
build `f5027fd`. Rebuild via `gh workflow run "Build artifacts (obi + collector)"
--repo wangzlei/opentelemetry-ebpf-instrumentation --ref demo/ping`, then redeploy
(pull new tag, retag `obi-collector:ping`, `docker rm -f ping-collector` + `docker run`).

## Access a host / common ops

```bash
aws ssm start-session --target i-07334e30db77c8da8            # shell on a host
# run a command on all hosts:
aws ssm send-command --targets Key=tag:purpose,Values=ping-demo \
  --document-name AWS-RunShellScript --parameters 'commands=["docker ps"]'
```
Config is at `/opt/ping/collector-config.yaml` (mounted into the collector); app files
under `/opt/ping/app`. To push a new config: write the file, then `docker restart ping-collector`.

## Active fault (remove to return to baseline)

`i-0fa62fab8cd729d0c` (audit, 10.0.6.169) has 400ms egress netem on `ens5`:
```bash
aws ssm send-command --targets Key=InstanceIds,Values=i-0fa62fab8cd729d0c \
  --document-name AWS-RunShellScript --parameters 'commands=["tc qdisc del dev ens5 root"]'
```

## Verify it's working

Metrics (CloudWatch OTLP, PromQL — see NOTES §7): server/client RED per service, e.g.
`histogram_count(sum_over_time({__name__="http.server.request.duration","@resource.service.name"="payment-service"}[5m]))`.
Traces (X-Ray): CLIENT spans carry `peer.service.name`.

## Cost / teardown

8× t3.medium run continuously. To stop:
```bash
IDS=$(aws ec2 describe-instances --region us-west-2 --filters Name=tag:purpose,Values=ping-demo \
  Name=instance-state-name,Values=running --query 'Reservations[].Instances[].InstanceId' --output text)
aws ec2 terminate-instances --region us-west-2 --instance-ids $IDS       # or stop-instances to keep them
# optional: delete the Route 53 zone Z097757611V01PJRE00N6 and SG sg-0f6caf64203f751e2
```
