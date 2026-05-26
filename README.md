# karpenter-provider-rackspace-spot

A Karpenter cloud provider for [Rackspace Spot](https://spot.rackspace.com) — provisions `SpotNodePool` and `OnDemandNodePool` resources in a Cloudspace in response to unschedulable pods, with bid-price-aware scheduling.

## Status

Pre-alpha. The Karpenter-side flow works end-to-end on a real Cloudspace:

- Karpenter scheduler picks a Rackspace ServerClass; `Create()` mints a 1-replica `SpotNodePool` or `OnDemandNodePool` (1-pool-per-NodeClaim mapping); `Delete()` is idempotent
- `RackspaceSpotNodeClass` CRD with reconciler that validates the referenced Cloudspace, caches its region, and refreshes the eligible ServerClass list
- Helm chart ships the provider's CRD plus the upstream `karpenter.sh` CRDs
- Multi-arch container image built with [ko](https://ko.build); snapshot `:main` + `:sha-<commit>` published on every push to `main`, versioned tags on release

**Known gap before nodes can actually carry workloads:** Rackspace's managed cloud-controller sets `node.spec.providerID = openstack:///<vm-uuid>`, but our provider returns `rackspacespot://<cs>/<kind>/<nodeclaim-uid>`. Karpenter can't link the joining Node back to the NodeClaim, so the NodeClaim never reaches `Ready` and Karpenter eventually disrupts it. The Node also lacks well-known labels (`karpenter.sh/nodepool`, `karpenter.sh/capacity-type`, `node.kubernetes.io/instance-type` → set by Rackspace's CCM to its internal VM SKU, not the ServerClass). Both need a post-registration reconciler — tracked separately.

Deferred — tracked as GitHub issues:

- [#1](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/issues/1) Drift detection (`IsDrifted`)
- [#2](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/issues/2) Preemption webhook receiver (graceful drain on Rackspace's 6-minute notice)
- [#3](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/issues/3) Repair policies (NotReady/unreachable Node replacement)
- [#4](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/issues/4) Rate-limit / quota handling for 1-pool-per-NodeClaim churn
- [#5](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/issues/5) Multi-Cloudspace operation
- [#8](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/issues/8) Embedded pricing snapshot + scheduled refresh PR

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

A minimal NodeClass + NodePool pair that provisions spot capacity in a Cloudspace:

```yaml
apiVersion: karpenter.rackspace.com/v1
kind: RackspaceSpotNodeClass
metadata: {name: default}
spec:
  cloudspaceName: my-cloudspace   # the target Cloudspace (region is derived)
  bidPrice: "0.005"               # required when capacity-type=spot
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

### Authentication

The controller reads `SPOT_REFRESH_TOKEN` (and other `SPOT_*` env vars per [spot-go-sdk Config](https://github.com/rackspace-spot/spot-go-sdk/blob/main/api/v1/client.go)). The chart wires this from `spot.refreshToken` or `spot.existingSecret`.

## Develop

```sh
make generate     # regenerate CRDs + deepcopy; sync chart crds/
make build        # build the controller binary
make test         # unit tests (mocks via spot-go-sdk/api/v1/mocks)
make image        # ko build + push (set KO_DOCKER_REPO and TAG)
make chart-lint   # helm lint
```

CI on PRs runs `make build`, `make test`, `make chart-lint`, `make chart-template`. The `Snapshot` workflow pushes `ghcr.io/kanya-approve/karpenter-provider-rackspace-spot:main` + `:sha-<commit>` on every push to `main`; tag releases (`v*.*.*`) publish versioned images and a Helm chart to `oci://ghcr.io/kanya-approve/charts`.

## Architecture notes

- **1 Rackspace pool per NodeClaim, `desired_server_count=1`.** Rackspace's smallest provisioning unit is a pool with a replica count; Karpenter wants per-machine lifecycle. This mapping gives clean Karpenter semantics at the cost of higher pool count under churn (see issue [#4](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/issues/4)).
- **Cloudspace is single-region.** Each `RackspaceSpotNodeClass` references one Cloudspace; the region is whatever the Cloudspace is in (cached in `status.region`, never user-set).
- **Pool name = NodeClaim UID.** Rackspace's admission webhook requires lowercase-UUID names. Karpenter-managed pools are distinguished by the `karpenter.rackspace.com/managed=true` label rather than a name prefix.
- **`karpenter.sh/v1` core CRDs ship with the chart.** The binary embeds `sigs.k8s.io/karpenter` and runs its core controllers (provisioning, disruption, nodeclaim, nodepool) alongside our `RackspaceSpotNodeClass` reconciler.

## License

Apache-2.0. See `LICENSE`.
