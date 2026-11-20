# OpenEverest provider for Neo4j

> [!WARNING]
> Alpha. Schemas and defaults may change without an upgrade path. Use the release built for
> your OpenEverest version; see [Compatibility](#compatibility).

[![CI](https://github.com/openeverest/provider-neo4j/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/openeverest/provider-neo4j/actions/workflows/ci.yaml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

Runs [Neo4j](https://neo4j.com/) through
[OpenEverest](https://github.com/openeverest/openeverest), using the
[Neo4j Kubernetes Operator](https://github.com/neo4j-partners/neo4j-kubernetes-operator).
The provider translates OpenEverest `Instance`, `Backup`, and `Restore` resources into the
operator's `Neo4j`, `Neo4jBackup`, `Neo4jBackupSchedule`, and `Neo4jRestore` resources.

## Capabilities

| Capability | Support |
|---|---|
| Standalone | Community and Enterprise |
| Cluster | Enterprise, with primary, analytics, and read pools |
| Horizontal / vertical scaling | Yes, within the operator's supported transitions |
| Storage expansion | Yes |
| TLS | Enabled by default; provider-managed CA or BYO Secret/Issuer |
| Prometheus metrics | Enterprise, activated by a monitoring destination or `spec.monitoring.sources.metrics.policy: Always` (rejected on Community) |
| On-demand and scheduled backups | Enterprise, S3-compatible BackupStorage |
| Restore / initial data-source seed | Yes; point-in-time recovery is not exposed |

Neo4j Enterprise licensing is opt-in. Set `spec.parameters.edition: enterprise` and
`spec.parameters.licenseAcceptance: "yes"` for production or `eval` for evaluation. Community
is the default and rejects cluster, backup, and Prometheus features. OpenEverest never supplies
or bypasses a commercial Neo4j license.

## Compatibility

| provider-neo4j | OpenEverest | Neo4j Kubernetes Operator | Kubernetes |
|---|---|---|---|
| `0.1.x` | `2.0.0-alpha.1` | `1.1.x` | `1.34`+ |

Each row names the exact OpenEverest version: SemVer ranks `2.0.0-dev.N` above
`2.0.0-alpha.1`, so a range would also claim the developer previews, whose Instance status
this provider does not write.

## Requirements

- OpenEverest `2.0.0-alpha.1`
- Kubernetes 1.34 or newer (required by the pinned Neo4j operator)
- cert-manager for the default TLS setup
- A default StorageClass unless every Instance selects one explicitly

## Installation

The provider chart bundles the Neo4j operator chart and the pinned operator CRDs. Install a
release from GHCR:

```bash
helm upgrade --install provider-neo4j \
  oci://ghcr.io/openeverest/charts/provider-neo4j --version 0.1.0 \
  --namespace everest-system --create-namespace
```

Or from a checkout:

```bash
helm dependency build charts/provider-neo4j
helm upgrade --install provider-neo4j charts/provider-neo4j \
  --namespace everest-system --create-namespace
```

The operator watches `default` by default. Set `operator.watchNamespaces` to the namespaces
where OpenEverest Instances run, or set `operator.clusterWide: true` together with
`operator.watchNamespaces: []` to watch every namespace. The operator namespace itself is
deliberately excluded by the upstream controller.

## Presets

The chart installs a `ClusterInstancePreset` per topology. `neo4j-standalone` (Community) is
enabled. `neo4j-cluster` needs Enterprise, whose license only you can accept, so it ships
disabled: enable it in your values and set `spec.parameters.licenseAcceptance`.

```yaml
presets:
  - name: standalone
    enabled: true
    spec: { ... }          # copy from the chart's values.yaml
  - name: cluster
    enabled: true
    spec:
      parameters:
        edition: enterprise
        licenseAcceptance: "yes"
      # ...rest copied from the chart's values.yaml
```

Helm replaces lists as a whole, so copy both entries when overriding `presets`.

## Usage

Community Standalone:

```yaml
apiVersion: core.openeverest.io/v1alpha1
kind: Instance
metadata:
  name: graph
spec:
  providerRef:
    name: neo4j
  topology:
    type: standalone
  version: "2026.07.1"
  components:
    engine:
      replicas: 1
      resources:
        limits:
          cpu: "1"
          memory: 2Gi
      storage:
        size: 10Gi
```

Enterprise Cluster:

```yaml
apiVersion: core.openeverest.io/v1alpha1
kind: Instance
metadata:
  name: graph-cluster
spec:
  providerRef:
    name: neo4j
  topology:
    type: cluster
  version: "2026.07.1"
  parameters:
    edition: enterprise
    licenseAcceptance: "yes"
  components:
    engine:
      replicas: 3
      resources:
        limits:
          cpu: "1"
          memory: 2Gi
      storage:
        size: 10Gi
    analytics:
      replicas: 1
```

The UI takes CPU in cores, fractions included (`0.5`), and memory and disk in Gi, and writes
them as Kubernetes quantity strings; a value it did not change keeps its stored form (for
example `2048Mi`). Disk size cannot be decreased.

## Status

The provider reports what it observes and OpenEverest derives the Instance's phase
(`Pending`, `Provisioned`, `Terminating`) and conditions from it:

| Fact | How the provider determines it |
|---|---|
| Converged (`Ready`) | The operator reports `Ready` for the latest generation, runs `spec.version`, has no upgrade in flight, and every StatefulSet has rolled out. |
| `Available` | Standalone: the server pod is ready. Cluster: the cluster is formed and a majority of primaries is ready. Unavailable during a restore or offline maintenance. |
| `Upgrading` | The operator's rolling upgrade is in flight (`status.upgrade`). |
| `StorageResizing` | The operator reports `StorageReady` with reason `StorageResizing`. |
| `Restoring` | A `Restore` targeting the Instance, or the data-source seed, is running. |
| Problems | The operator's `Error` condition (version refusals and storage template drift are terminal), failed TLS, storage, plugin or cluster-formation conditions, a failed upgrade, servers lost after being ready, and `spec.maintenance.offlineMode` set outside OpenEverest (terminal). |

The provider labels each pool's pods with the OpenEverest Instance and component labels, so
`Instance.status.components` reports ready and desired pods per component, and metrics
endpoints select pods by component.

Once the Instance is available or converged, the provider writes connection details to the
Secret referenced by `Instance.status.connectionSecretRef`. Its `ca.crt` entry is the CA for
the generated or supplied TLS certificate.

See [examples/](examples/) and the source-of-truth definitions in [definition/](definition/).

## Development

```bash
make generate
make test-unit
make helm-lint
```

Integration tests use Chainsaw. `make install-crds` installs the OpenEverest CRDs of the core
version pinned in `go.mod`; CI builds the OpenEverest controller from the same version:

```bash
make install-crds
make deploy-provider-ci
make test-integration            # every suite, or one of:
make test-integration-core       # standalone, cluster, admission webhook
make test-integration-presets    # the chart's ClusterInstancePresets
make test-integration-monitoring # spec.monitoring.sources.metrics.policy
```

The Neo4j operator is scaled to 0 in these suites: each writes the status the operator would
report, and asserts what the provider renders and reports.

Run `make verify` before committing generated files. The pinned Neo4j operator is `v1.1.0`;
CRDs in `charts/provider-neo4j/crds/` are copied from that version by `make generate`.

## License

Apache License 2.0. See [LICENSE](LICENSE). Neo4j Community and Enterprise images retain
their respective upstream licenses.
