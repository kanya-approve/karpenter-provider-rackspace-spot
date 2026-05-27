# Karpenter Provider for Rackspace Spot

[![License](https://img.shields.io/github/license/kanya-approve/karpenter-provider-rackspace-spot)](LICENSE)
[![Release](https://img.shields.io/github/v/release/kanya-approve/karpenter-provider-rackspace-spot?sort=semver)](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/kanya-approve/karpenter-provider-rackspace-spot/ci.yml?branch=main)](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/kanya-approve/karpenter-provider-rackspace-spot)](https://goreportcard.com/report/github.com/kanya-approve/karpenter-provider-rackspace-spot)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/karpenter-provider-rackspace-spot)](https://artifacthub.io/packages/search?repo=karpenter-provider-rackspace-spot)

A [Karpenter](https://karpenter.sh) cloud provider for [Rackspace Spot](https://spot.rackspace.com). Provisions `SpotNodePool` and `OnDemandNodePool` resources in a Cloudspace in response to unschedulable pods, with smart bid pricing driven by Rackspace's published percentile feed.

**Status:** Alpha — driven end-to-end on a real Cloudspace, but APIs and chart values may shift before v1.0.

## Requirements

- A Rackspace Spot account and at least one Cloudspace (the target Kubernetes cluster).
- A Rackspace Spot OAuth refresh token (from your account profile in the [Rackspace Spot console](https://spot.rackspace.com)).
- `helm` 3.8+ (for OCI chart support).
- Kubernetes v1.31+ inside the Cloudspace.

| Provider | Karpenter core | Kubernetes | Rackspace Spot SDK    |
| -------- | -------------- | ---------- | --------------------- |
| v0.1.x   | v1.12.x        | v1.31+     | `spot-go-sdk` v0.2+   |

## Installation

Grab a refresh token from your Rackspace Spot account page, then install with one of:

```sh
# inline (token from env var or pasted directly)
helm install karpenter \
  oci://ghcr.io/kanya-approve/charts/karpenter-provider-rackspace-spot \
  --version 0.1.0 \
  --namespace karpenter --create-namespace \
  --set spot.refreshToken=$RXTSPOT_REFRESH_TOKEN
```

```sh
# pre-existing secret
kubectl create namespace karpenter
kubectl -n karpenter create secret generic karpenter-spot \
  --from-literal=refreshToken=$RXTSPOT_REFRESH_TOKEN

helm install karpenter \
  oci://ghcr.io/kanya-approve/charts/karpenter-provider-rackspace-spot \
  --version 0.1.0 \
  --namespace karpenter \
  --set spot.existingSecret=karpenter-spot
```

The chart ships the provider's `RackspaceSpotNodeClass` CRD and the upstream `karpenter.sh/v1` `NodePool` + `NodeClaim` CRDs.

For development against the source tree, swap `oci://...` for `charts/karpenter` and `--set image.tag=main`.

Snapshot images (`:main`, `:sha-<commit>`) ship on every push to `main`; snapshot charts (`<chart-version>-main.<short-sha>`) ship when `charts/**` changes. Tag releases (`v*.*.*`) publish plain-versioned images and charts.

## Documentation

- [Configuration](#configuration)
- [Bidding](#bidding)
- [Workload affinity](#workload-affinity)
- [Node repair (opt-in)](#node-repair-opt-in)
- [Authentication](#authentication)
- [Architecture notes](#architecture-notes)
- [Development](#development)

## Configuration

A minimal NodeClass + NodePool pair that provisions spot capacity:

```yaml
apiVersion: karpenter.rackspace.com/v1
kind: RackspaceSpotNodeClass
metadata: {name: default}
spec:
  cloudspaceName: my-cloudspace   # the target Cloudspace (region is derived)
  bidPercentile: P80              # P20 | P50 | P80 (default P80). See "Bidding".
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: default}
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.rackspace.com
        kind: RackspaceSpotNodeClass
        name: default
      requirements:
        - {key: karpenter.sh/capacity-type, operator: In, values: ["spot"]}
        - {key: kubernetes.io/arch,         operator: In, values: ["amd64"]}
        - {key: kubernetes.io/os,           operator: In, values: ["linux"]}
      expireAfter: 720h
  limits: {cpu: "100", memory: 400Gi}
  disruption: {consolidationPolicy: WhenEmptyOrUnderutilized, consolidateAfter: 30s}
```

Working manifests live in `config/samples/`.

## Bidding

Every spot pool's bid is computed at Create time as `max(current_market, percentile) × 1.05`, clamped up to the ServerClass's `minBidPrice` floor and rounded to Rackspace's precision rules (≤3 decimal places, or multiples of $0.01 above $0.05). There is no user-set `bidPrice` field on the NodeClass — the only knob is which percentile of Rackspace's published 30-day distribution to bid at:

- `P80` (default): bid clears ~80% of typical price ticks. Stable, more expensive on volatile SKUs.
- `P50`: bid clears ~50%. Cheaper, more eviction-prone.
- `P20`: cheapest, most eviction-prone.

Cost ceilings should be expressed via `NodePool.limits` or NodePool requirements on `node.kubernetes.io/instance-type`, not via a per-pool bid.

Pricing data comes from [Rackspace's public S3 feed](https://ngpc-prod-public-data.s3.us-east-2.amazonaws.com/percentiles.json). The controller fetches it live (5-min cache) and falls back to an embedded snapshot at `pkg/providers/pricing/initial-prices.json` (refreshed weekly via `.github/workflows/update-pricing.yaml`).

## Workload affinity

The Rackspace/OpenStack cloud-controller overwrites `node.kubernetes.io/instance-type` (with the underlying VM SKU, e.g. `compute1-4`) and `topology.kubernetes.io/region` (with the OpenStack region code, e.g. `HKG`) post-registration. Workloads using `nodeSelector` or affinity should target Karpenter-set labels instead:

- `karpenter.sh/nodepool=<name>` — which NodePool spawned the node
- `karpenter.sh/capacity-type=spot|on-demand` — what kind of capacity
- `karpenter.rackspace.com/managed=true` — Karpenter-provisioned (vs. seed/manual pools)
- `karpenter.rackspace.com/rackspacespotnodeclass=<name>` — which NodeClass

Don't filter on `node.kubernetes.io/instance-type=<ServerClass>` — the CCM clobbers it.

## Node repair (opt-in)

`RepairPolicies` returns a non-empty list (`Ready=False/Unknown` or `NetworkUnavailable=True` for 30+ min triggers replacement), but Karpenter's `node.health` controller is gated behind the `NodeRepair` feature gate, **off by default**. A transient kubelet hiccup or control-plane blip can briefly flip many nodes to `Ready=Unknown`, and an over-eager repair controller would race to wipe them; the conservative default leaves that decision to the operator.

To opt in:

```sh
helm upgrade karpenter ... \
  --set 'extraEnv[0].name=FEATURE_GATES' \
  --set 'extraEnv[0].value=NodeRepair=true\,ReservedCapacity=true\,SpotToSpotConsolidation=false\,NodeOverlay=false\,StaticCapacity=false'
```

The full set has to be passed because `FEATURE_GATES` is parsed as a complete override, not a partial merge.

## Authentication

The controller reads `SPOT_REFRESH_TOKEN` (and other `SPOT_*` env vars per [spot-go-sdk Config](https://github.com/rackspace-spot/spot-go-sdk/blob/main/api/v1/client.go)). The chart wires this from `spot.refreshToken` or `spot.existingSecret`.

## What's verified

| Flow | Notes |
| ---- | ----- |
| Scale up | spot + on-demand, single + multi-replica, mixed shapes |
| Scale down | consolidation drains via eviction API, deletes the pool |
| Smart bidding | percentile knob + `minBidPrice` floor clamp + precision rounding |
| External pool delete recovery | `registrationTimeout` triggers `Delete`, NodeClaim replaced |
| Node repair | Behind `NodeRepair` feature gate — see above |

## What's not yet in

- **[#2](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/issues/2) Preemption webhook receiver** — Rackspace gives ~6 min notice via a push webhook; we don't run a receiver yet, so spot evictions are ungraceful (pods get hard-killed when the server vanishes).

## Architecture notes

- **1 Rackspace pool per NodeClaim, `desired_server_count=1`.** Rackspace's smallest provisioning unit is a pool with a replica count; Karpenter wants per-machine lifecycle. This mapping gives clean Karpenter semantics at the cost of higher pool count under churn.
- **Cloudspace is single-region.** Each `RackspaceSpotNodeClass` references one Cloudspace; the region is whatever the Cloudspace is in (cached in `status.region`, never user-set). Multi-Cloudspace deployments run one Karpenter install per Cloudspace.
- **Pool name = NodeClaim UID.** Rackspace's admission webhook requires lowercase-UUID names. Karpenter-managed pools are distinguished by the `karpenter.rackspace.com/managed=true` label rather than a name prefix.
- **`karpenter.sh/v1` core CRDs ship with the chart.** The binary embeds `sigs.k8s.io/karpenter` and runs its core controllers (provisioning, disruption, nodeclaim, nodepool) alongside our `RackspaceSpotNodeClass` reconciler and the `nodelink` reconciler.
- **`nodelink` reconciler bridges the providerID gap.** Rackspace's CCM sets `Node.spec.providerID = openstack:///<vm-uuid>`; our `Create()` returns `rackspacespot://<cs>/<kind>/<nodeclaim-uid>` because we don't know the VM UUID at create time (Rackspace's auction decouples bid acceptance from server assignment). When a Node joins carrying our `karpenter.rackspace.com/managed=true` label, `nodelink` patches the NodeClaim's `Status.ProviderID` to match the CCM-set value so Karpenter's lifecycle controller can complete the binding.

## Development

```sh
make generate       # regenerate CRDs + deepcopy; sync chart crds/
make build          # build the controller binary
make test           # unit tests (mocks via spot-go-sdk/api/v1/mocks)
make image          # ko build + push (set KO_DOCKER_REPO and TAG)
make chart-lint     # helm lint
make update-pricing # refresh the embedded pricing snapshot from the S3 feed
```

CI runs `make build`, `make test`, `make chart-lint`, `make chart-template` on every PR.

## Community

- Karpenter discussion and troubleshooting: [#karpenter](https://kubernetes.slack.com/archives/C02SFFZSA2K) in the [Kubernetes Slack](https://slack.k8s.io/).
- Bug reports and feature requests: [GitHub Issues](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/issues).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the pull-request workflow and dev setup.

## Code of Conduct

Participation in this project is governed by the [Code of Conduct](CODE_OF_CONDUCT.md), adapted from Contributor Covenant 1.4.

## References

- [Karpenter](https://karpenter.sh) — the upstream autoscaler this provider plugs into.
- [Rackspace Spot](https://spot.rackspace.com) — the platform we provision against.
- [spot-go-sdk](https://github.com/rackspace-spot/spot-go-sdk) — official Go SDK for Rackspace Spot.
- Layout and conventions modeled after [`oracle/karpenter-provider-oci`](https://github.com/oracle/karpenter-provider-oci), [`aws/karpenter-provider-aws`](https://github.com/aws/karpenter-provider-aws), and [`sergelogvinov/karpenter-provider-proxmox`](https://github.com/sergelogvinov/karpenter-provider-proxmox).

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
