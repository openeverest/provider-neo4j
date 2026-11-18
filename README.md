# OpenEverest provider for Neo4j

> [!WARNING]
> Pre-alpha. Schemas and defaults may change without an upgrade path.

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
| Prometheus metrics | Enterprise, activated by OpenEverest monitoring |
| On-demand and scheduled backups | Enterprise, S3-compatible BackupStorage |
| Restore / initial data-source seed | Yes; point-in-time recovery is not exposed |

Neo4j Enterprise licensing is opt-in. Set `spec.parameters.edition: enterprise` and
`spec.parameters.licenseAcceptance: "yes"` for production or `eval` for evaluation. Community
is the default and rejects cluster, backup, and Prometheus features. OpenEverest never supplies
or bypasses a commercial Neo4j license.

## Requirements

- OpenEverest v2
- Kubernetes 1.34 or newer (required by the pinned Neo4j operator)
- cert-manager for the default TLS setup
- A default StorageClass unless every Instance selects one explicitly

## Installation

The provider chart bundles the Neo4j operator chart and the pinned operator CRDs:

```bash
helm dependency build charts/provider-neo4j
helm upgrade --install provider-neo4j charts/provider-neo4j \
  --namespace everest-system --create-namespace
```

The operator watches `default` by default because the upstream `1.0.0` chart does not
wire its cluster-wide value to `WATCH_ALL_NAMESPACES`. Set `operator.watchNamespaces`
to the namespaces where OpenEverest Instances run. The operator namespace itself is
deliberately excluded by the upstream controller.

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

When the operator marks the `Neo4j` resource Running, the provider writes connection details
to the Secret referenced by `Instance.status.connectionSecretRef`. Its `ca.crt` entry is the CA
for the generated or supplied TLS certificate.

See [examples/](examples/) and the source-of-truth definitions in [definition/](definition/).

## Development

```bash
make generate
make test-unit
make helm-lint
```

Integration tests use Chainsaw:

```bash
make install-crds
make deploy-provider-ci
make test-integration-core
```

Run `make verify` before committing generated files. The pinned Neo4j operator is `v1.0.0`;
CRDs in `charts/provider-neo4j/crds/` are copied from that version by `make generate`.

## License

Apache License 2.0. See [LICENSE](LICENSE). Neo4j Community and Enterprise images retain
their respective upstream licenses.
