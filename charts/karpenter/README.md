# karpenter-provider-rackspace-spot

Helm chart for the [Karpenter](https://karpenter.sh) cloud provider that provisions `SpotNodePool` and `OnDemandNodePool` resources inside a [Rackspace Spot](https://spot.rackspace.com) Cloudspace.

Each Helm install runs one operator against one Cloudspace. Cloudspaces are single-region and each Cloudspace is its own Kubernetes cluster, so the chart is single-tenant by design.

## Install

```sh
helm install karpenter \
  oci://ghcr.io/kanya-approve/charts/karpenter-provider-rackspace-spot \
  --version 0.1.0 \
  --namespace karpenter --create-namespace \
  --set spot.cloudspaceName=my-cloudspace \
  --set spot.refreshToken=$RXTSPOT_REFRESH_TOKEN
```

Or pre-create a Secret and reference it:

```sh
kubectl -n karpenter create secret generic karpenter-spot \
  --from-literal=refreshToken=$RXTSPOT_REFRESH_TOKEN

helm install karpenter \
  oci://ghcr.io/kanya-approve/charts/karpenter-provider-rackspace-spot \
  --version 0.1.0 \
  --namespace karpenter --create-namespace \
  --set spot.cloudspaceName=my-cloudspace \
  --set spot.existingSecret=karpenter-spot
```

The chart bundles its own `RackspaceSpotNodeClass` CRD plus the upstream `karpenter.sh/v1` `NodePool` and `NodeClaim` CRDs.

## Required values

| Key                   | Notes |
| --------------------- | ----- |
| `spot.cloudspaceName` | Target Cloudspace name. The operator panics on startup if unset. |
| `spot.refreshToken`   | Rackspace Spot OAuth refresh token. Alternatively, `spot.existingSecret` referencing a Secret with key `refreshToken`. |

## Common values

| Key                                  | Default                                                          | Notes |
| ------------------------------------ | ---------------------------------------------------------------- | ----- |
| `image.repository`                   | `ghcr.io/kanya-approve/karpenter-provider-rackspace-spot`        | |
| `image.tag`                          | `""` (defaults to `Chart.appVersion`)                            | |
| `replicaCount`                       | `1`                                                              | |
| `resources.requests.cpu`             | `200m`                                                           | |
| `resources.requests.memory`          | `256Mi`                                                          | |
| `operator.disableLeaderElection`     | `false`                                                          | |
| `extraEnv`                           | `[]`                                                             | Pass-through to controller (e.g. `FEATURE_GATES=NodeRepair=true`). |

See [`values.yaml`](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/blob/main/charts/karpenter/values.yaml) for the full list.

## Enabling node repair

`RepairPolicies` are wired up but Karpenter's `node.health` controller is gated behind the `NodeRepair` feature gate, off by default. To opt in, pass the full `FEATURE_GATES` string via `extraEnv`:

```sh
--set 'extraEnv[0].name=FEATURE_GATES' \
--set 'extraEnv[0].value=NodeRepair=true\,ReservedCapacity=true\,SpotToSpotConsolidation=false\,NodeOverlay=false\,StaticCapacity=false'
```

## Documentation

Full provider documentation, including the NodeClass / NodePool schema, bidding behavior, and workload-affinity caveats, lives in the [project README](https://github.com/kanya-approve/karpenter-provider-rackspace-spot#readme).
