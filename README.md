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
| TLS | Required by default; provider-managed CA, your cert-manager issuer (bring your own CA) or certificate Secret |
| Prometheus metrics | Enterprise, activated by a monitoring destination or `spec.monitoring.sources.metrics.policy: Always` (rejected on Community) |
| On-demand and scheduled backups | Enterprise, S3-compatible BackupStorage |
| Restore / initial data-source seed | Yes; point-in-time recovery is not exposed |
| Backup import | Yes; `BackupImport` finds Neo4j backup artifacts (`<database>-<time>.backup`) in a BackupStorage |

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

The bundled Neo4j operator watches every namespace, which lets it read Secrets and bind RBAC
cluster-wide. To restrict it, list the namespaces where OpenEverest Instances run:

```yaml
operator:
  clusterWide: false
  watchNamespaces: [team-a, team-b]
```

An Instance in a namespace the operator does not watch gets no pods and no error. The upstream
controller never watches its own namespace, so do not create Instances in the release
namespace.

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
  version: "2026.10.2"
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
  version: "2026.10.2"
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

### Secondary pools

Analytics and read servers host only the databases whose topology asks for secondaries. The
Neo4j operator does not widen the topology of an existing database, so the default `neo4j`
database stays on the primaries. Place it on the pools yourself, with the total of analytics
and read members as the secondaries:

```cypher
// on the system database, for 3 primaries, 1 analytics and 1 read server
ALTER DATABASE neo4j SET TOPOLOGY 3 PRIMARIES 2 SECONDARIES
```

Run it again after adding members to a pool; on a scale-in, the operator narrows the topology
itself. Databases you create later get the pools by default.

### High availability

A Cluster gets two defaults:

- a PodDisruptionBudget that lets a node drain take one member at a time, across all pools;
- a preference to run each member of a pool on its own node (soft pod anti-affinity).
  `spec.components.engine.schedulingPolicy.affinity` replaces it.

The preference is part of the pod template. A cluster created by provider 0.1.x does not
get it, because adding it would restart every member.

### External access

`spec.components.engine.service.serviceType` exposes Bolt, HTTP and HTTPS through a
`ClusterIP` (default), `NodePort` or `LoadBalancer` Service:

```yaml
components:
  engine:
    service:
      serviceType: LoadBalancer
      loadBalancerService:
        sourceRanges: ["203.0.113.0/24"]
```

A LoadBalancer without `sourceRanges` is open to `0.0.0.0/0`: the Neo4j operator requires
source ranges, and OpenEverest treats unset ranges as unrestricted. Set them for anything
reachable from the internet; the UI does not offer the field yet.

### Configuration

`neo4j.conf` settings are a map under the engine's parameters. The UI has no editor for it;
set it in the Instance:

```yaml
components:
  engine:
    parameters:
      configuration:
        db.tx_log.rotation.retention_policy: "2 days"
```

The UI takes CPU in cores, fractions included (`0.5`), and memory and disk in Gi, and writes
them as Kubernetes quantity strings; a value it did not change keeps its stored form (for
example `2048Mi`). Disk size cannot be decreased.

### TLS

TLS is set instance-wide under `spec.parameters.tls`, in the shape every OpenEverest provider
shares:

```yaml
spec:
  parameters:
    tls:
      mode: requireTLS                 # requireTLS (default) | disabled
      issuer:                          # a cert-manager issuer, or ...
        name: company-ca
        kind: ClusterIssuer            # Issuer (default) | ClusterIssuer
      # certificateSecretName: graph-tls  # ... a Secret with tls.crt, tls.key and ca.crt
      additionalNames:                 # DNS names clients use, besides the in-cluster ones
        - graph.company.com
```

| Field | Neo4j operator |
|---|---|
| `mode: requireTLS` | `trust.enabled`; Bolt (`server.bolt.tls_level=REQUIRED`), HTTPS and, on a Cluster, member traffic use TLS |
| `mode: disabled` | `trust.enabled: false`; Standalone only, for evaluation |
| `issuer` | `trust.certManager.issuerRef` |
| `certificateSecretName` | `trust.certificates.{bolt,https,cluster}` key and certificate references |
| `additionalNames` | `trust.certManager.dnsNames` (Bolt and HTTPS certificates) |

The certificates come from, in order of precedence:

1. `certificateSecretName`: a Secret in the instance namespace with `tls.crt`, `tls.key` and
   `ca.crt`, labelled `neo4j.com/mountable=true`. Its certificate must cover the client Service
   (`<name>.<namespace>.svc.cluster.local`) and, on a Cluster, the member Services.
2. `issuer`: a cert-manager `Issuer` or `ClusterIssuer` that writes its CA to `ca.crt`, as the CA
   and Vault issuers do. Only the `cert-manager.io` group works: the Neo4j operator's
   `issuerRef` has no group, so external issuers (AWS PCA, Google CAS) are rejected.
3. Otherwise the provider creates a CA for the instance through cert-manager.

Rules:

- `preferTLS` is not offered: the Neo4j operator always sets `server.bolt.tls_level=REQUIRED`
  once Bolt has a certificate.
- TLS can be turned on later on a Standalone, never off. The certificate source can't change
  while TLS is on.
- `additionalNames` are DNS names (wildcards allowed, no IPs) and can't be combined with
  `certificateSecretName`, whose certificate carries its own names. Changing them on a running
  instance reissues the certificates and restarts the pods, so it waits for maintenance
  approval: the Instance lists a `tls-names-…` action in `status.pendingMaintenance`; copy its
  token to `spec.maintenance.approved`. The UI doesn't edit `additionalNames` yet.
- The connection Secret's `ca.crt` is the CA of the Bolt certificate.

#### Bring your own CA

Give cert-manager your company CA and point the instance at it:

```sh
kubectl -n cert-manager create secret tls company-ca --cert=ca.crt --key=ca.key
```

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: company-ca
spec:
  ca:
    secretName: company-ca
---
apiVersion: core.openeverest.io/v1alpha1
kind: Instance
metadata:
  name: graph
spec:
  providerRef:
    name: neo4j
  topology:
    type: standalone
  version: "2026.10.2"
  parameters:
    tls:
      issuer:
        name: company-ca
        kind: ClusterIssuer
      additionalNames: [graph.company.com]
  components:
    engine:
      replicas: 1
      service:
        serviceType: LoadBalancer
      storage:
        size: 10Gi
```

Point `graph.company.com` at the LoadBalancer address; clients that trust the company CA connect
with full verification (`neo4j+s://graph.company.com:7687`).

#### Migrating from provider 0.x TLS settings

`mode: enabled` is replaced by `requireTLS` (the same behaviour). `issuer` and
`certificateSecretName` keep their meaning. An Instance that still sets `mode: enabled` goes
`ValidationFailed` after the upgrade; its Neo4j keeps running unchanged until you migrate it:

```sh
kubectl patch instance <name> -n <namespace> --type=merge \
  -p '{"spec":{"parameters":{"tls":{"mode":"requireTLS"}}}}'
```

Instances that never set `mode` are unaffected.

## Backup parameters

`parameters` on a `Backup`, or on a schedule in `spec.backup.storages[].schedules[]`:

| Parameter | Backup | Schedule | Meaning |
|---|---|---|---|
| `databases` | yes | yes | Databases to back up. Default `["neo4j"]`. |
| `type` | yes | no | `Full` (default), `Auto` or `Incremental`. A schedule's cron always takes full backups. |
| `incrementalSchedule` | no | yes | Cron for incremental backups between the schedule's full backups. |
| `aggregate` | no | yes | Compact each chain into one full backup once the next full backup closes it. |
| `scratchSize` | yes | yes | Dedicated staging volume, for example `200Gi`. Set it for large databases. |

```yaml
schedules:
  - name: weekly
    enabled: true
    cron: "0 2 * * 0"                      # full backup on Sunday
    retention: {type: count, count: 4}     # keeps four chains
    parameters:
      incrementalSchedule: "0 2 * * 1-6"   # incrementals on the other days
      aggregate: true
```

Retention counts chains: a full backup together with its incrementals.

## Restore parameters

`parameters` on a `Restore`:

| Parameter | Default | Meaning |
|---|---|---|
| `databases` | `["neo4j"]` | Databases to restore. |
| `overwrite` | `true` | Replace databases that already exist. Every Neo4j has `neo4j`, so `false` restores only databases the Instance does not have yet. |
| `forceOffline` | `false` | Stop a database before replacing it. Requires `overwrite`. |

A restore into a new Instance (`spec.dataSource`) always overwrites and forces offline: it
replaces the empty default database the new Instance starts with.

## Importing backups

A `BackupImport` lists a BackupStorage and creates a `Backup` for every Neo4j backup artifact
(`<database>-<time>.backup`) under its prefix, including ones written by another cluster or
by `neo4j-admin database backup`. Restore one like any other Backup, in place or as a new
Instance's `spec.dataSource`. The Instance must have the storage in `spec.backup.storages`,
because its servers read the artifact with that storage's credentials.

An imported Backup holds one database and restores into the database it was taken from; set
`parameters.databases` on the Restore to restore it under another name. The provider never
deletes imported artifacts.

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
the Bolt certificate, whichever source issued it.

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
make test-integration-core       # standalone, cluster, exposure, admission webhook
make test-integration-presets    # the chart's ClusterInstancePresets
make test-integration-monitoring # spec.monitoring.sources.metrics.policy
```

The Neo4j operator is scaled to 0 in these suites: each writes the status the operator would
report, and asserts what the provider renders and reports.

The e2e-cluster suites run a real Neo4j through the operator and query it over Bolt:

```bash
make deploy-cert-manager
make deploy-provider-e2e
make test-e2e-cluster                 # every suite, or one of:
make test-e2e-cluster-standalone      # default TLS, version upgrade, LoadBalancer
make test-e2e-cluster-tls             # issuer, additionalNames maintenance, TLS rules
make test-e2e-cluster-backup-restore  # backup, in-place restore, seeding a new Instance
make test-e2e-cluster-cluster         # three primaries, adding a read pool
```

The backup and cluster suites run Neo4j Enterprise under its evaluation license
(`licenseAcceptance: eval`).

Run `make verify` before committing generated files. The pinned Neo4j operator is `v1.1.0`;
CRDs in `charts/provider-neo4j/crds/` are copied from that version by `make generate`.

Dependabot proposes new operator releases by bumping the subchart in `Chart.yaml`. It cannot
bump the `replace` directive that pins the operator's API module in `go.mod`, so finish its PR
by hand: set the same version there, run `go mod tidy` and `make generate`, and commit.

## License

Apache License 2.0. See [LICENSE](LICENSE). Neo4j Community and Enterprise images retain
their respective upstream licenses.
