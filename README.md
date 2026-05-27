# karpenter-provider-rackspace-spot

A Karpenter cloud provider for [Rackspace Spot](https://spot.rackspace.com) — provisions `SpotNodePool` and `OnDemandNodePool` resources in a Cloudspace in response to unschedulable pods, with smart bid pricing driven by Rackspace's published percentile feed.

## Status

Alpha. Driven end-to-end on a real Cloudspace; all core flows validated:

- **Scale up** (spot + on-demand, single + multi-replica, mixed-shape workloads)
- **Scale down** (consolidation drains nodes via the eviction API and deletes the underlying pool)
- **Smart bidding** (`max(market, P{20,50,80}) × 1.05`, clamped to Rackspace's per-ServerClass `minBidPrice` floor, rounded to Rackspace's 3-dp / 2-dp-above-$0.05 precision rules)
- **External pool delete recovery** (Karpenter's `registrationTimeout` triggers our `Delete`, the SDK returns 404, NodeClaim is cleaned up and replaced)
- **Drift detection** (NodeClass edits to labels/taints/annotations/cloudspace/serverClassSelector roll the affected nodes via a hash-annotation comparison)
- **Repair policies** (Node `Ready=False/Unknown` or `NetworkUnavailable=True` for 30+ min auto-replaces the node)

What's not in yet:

- **[#2](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/issues/2) Preemption webhook receiver** — Rackspace gives ~6 min notice via a push webhook; we don't run a receiver yet, so spot evictions are ungraceful (pods get hard-killed when the server vanishes).

## Install

No tagged release has been cut yet, so OCI install pulls a snapshot version. List what's published:

```sh
gh api /users/kanya-approve/packages/container/charts%2Fkarpenter-provider-rackspace-spot/versions \
  | jq -r '.[].metadata.container.tags[]'
```

Then install:

```sh
helm install karpenter \
  oci://ghcr.io/kanya-approve/charts/karpenter-provider-rackspace-spot \
  --version 0.1.0-main.<short-sha> \
  --namespace karpenter --create-namespace \
  --set spot.refreshToken="$(awk '/refreshToken:/{print $2}' ~/.spot_config)"
```

Or against the source tree for development:

```sh
helm install karpenter charts/karpenter \
  --namespace karpenter --create-namespace \
  --set image.tag=main \
  --set spot.refreshToken="$RXTSPOT_REFRESH_TOKEN"
```

Snapshot images (`:main`, `:sha-<commit>`) ship on every push to `main`; snapshot charts (`<chart-version>-main.<short-sha>`) ship only when `charts/**` changes. Once `git tag v0.1.0 && git push --tags` lands, the release workflow publishes a plain `0.1.0` image + chart.

The chart's `crds/` ships both the provider's `RackspaceSpotNodeClass` CRD and the upstream `karpenter.sh/v1` `NodePool` + `NodeClaim` CRDs.

## Configure

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

See `config/samples/` for working manifests.

### Bidding

Every spot pool's bid is computed at Create time as `max(current_market, percentile) × 1.05`, clamped up to the ServerClass's `minBidPrice` floor and rounded to Rackspace's precision rules (≤3 decimal places, or multiples of $0.01 above $0.05). There is no user-set `bidPrice` field on the NodeClass — the only knob is which percentile of Rackspace's published 30-day distribution to bid at:

- `P80` (default): bid clears ~80% of typical price ticks. Stable, more expensive on volatile SKUs.
- `P50`: bid clears ~50%. Cheaper, more eviction-prone.
- `P20`: cheapest, most eviction-prone.

Cost ceilings should be expressed via `NodePool.limits` or `RackspaceSpotNodeClass.serverClassSelector`, not via a per-pool bid.

Pricing data comes from [Rackspace's public S3 feed](https://ngpc-prod-public-data.s3.us-east-2.amazonaws.com/percentiles.json). The controller fetches it live (5-min cache) and falls back to an embedded snapshot at `pkg/providers/pricing/initial-prices.json` (refreshed weekly via `.github/workflows/update-pricing.yaml`).

### Workload affinity — use `karpenter.sh/*` labels

The Rackspace/OpenStack cloud-controller overwrites `node.kubernetes.io/instance-type` (with the underlying VM SKU, e.g. `compute1-4`) and `topology.kubernetes.io/region` (with the OpenStack region code, e.g. `HKG`) post-registration. Workloads using `nodeSelector` or affinity should target:

- `karpenter.sh/nodepool=<name>` — which NodePool spawned the node
- `karpenter.sh/capacity-type=spot|on-demand` — what kind of capacity
- `karpenter.rackspace.com/managed=true` — Karpenter-provisioned (vs. seed/manual pools)
- `karpenter.rackspace.com/rackspacespotnodeclass=<name>` — which NodeClass

Don't filter on `node.kubernetes.io/instance-type=<ServerClass>` — the CCM clobbers it.

### Authentication

The controller reads `SPOT_REFRESH_TOKEN` (and other `SPOT_*` env vars per [spot-go-sdk Config](https://github.com/rackspace-spot/spot-go-sdk/blob/main/api/v1/client.go)). The chart wires this from `spot.refreshToken` or `spot.existingSecret`.

## Develop

```sh
make generate     # regenerate CRDs + deepcopy; sync chart crds/
make build        # build the controller binary
make test         # unit tests (mocks via spot-go-sdk/api/v1/mocks)
make image        # ko build + push (set KO_DOCKER_REPO and TAG)
make chart-lint   # helm lint
make update-pricing # refresh the embedded pricing snapshot from the S3 feed
```

CI on PRs runs `make build`, `make test`, `make chart-lint`, `make chart-template`. The `Snapshot` workflow pushes `ghcr.io/kanya-approve/karpenter-provider-rackspace-spot:main` + `:sha-<commit>` on every push to `main`; tag releases (`v*.*.*`) publish versioned images and a Helm chart to `oci://ghcr.io/kanya-approve/charts`.

## Architecture notes

- **1 Rackspace pool per NodeClaim, `desired_server_count=1`.** Rackspace's smallest provisioning unit is a pool with a replica count; Karpenter wants per-machine lifecycle. This mapping gives clean Karpenter semantics at the cost of higher pool count under churn.
- **Cloudspace is single-region.** Each `RackspaceSpotNodeClass` references one Cloudspace; the region is whatever the Cloudspace is in (cached in `status.region`, never user-set). Multi-Cloudspace deployments run one Karpenter install per Cloudspace.
- **Pool name = NodeClaim UID.** Rackspace's admission webhook requires lowercase-UUID names. Karpenter-managed pools are distinguished by the `karpenter.rackspace.com/managed=true` label rather than a name prefix.
- **`karpenter.sh/v1` core CRDs ship with the chart.** The binary embeds `sigs.k8s.io/karpenter` and runs its core controllers (provisioning, disruption, nodeclaim, nodepool) alongside our `RackspaceSpotNodeClass` reconciler and the `nodelink` reconciler.
- **`nodelink` reconciler bridges the providerID gap.** Rackspace's CCM sets `Node.spec.providerID = openstack:///<vm-uuid>`; our `Create()` returns `rackspacespot://<cs>/<kind>/<nodeclaim-uid>` because we don't know the VM UUID at create time (Rackspace's auction decouples bid acceptance from server assignment). When a Node joins carrying our `karpenter.rackspace.com/managed=true` label, `nodelink` patches the NodeClaim's `Status.ProviderID` to match the CCM-set value so Karpenter's lifecycle controller can complete the binding.

## License

Apache-2.0. See `LICENSE`.
