```text
         __        _____
  _ __  / _| ___  |__  /
 | '_ \| |_ / __|   / /
 | | | |  _|\__ \  / /_
 |_| |_|_|  |___/ /____|
```

**Cross-namespace RWX volumes for Kubernetes — no external NFS server, ever.**

> ⚠️ **Heads up: this project was completely vibe coded** for a few small
> personal use-cases. It works great on my machine™, it has tests, and it
> still owes you zero guarantees. Use at your own risk, expect sharp edges,
> and please do **not** run it in production. If it eats your data, you get
> to keep both halves.

---

- [What is nfsZ?](#what-is-nfsz)
- [Is nfsZ right for you?](#is-nfsz-right-for-you)
- [How it works](#how-it-works)
- [Quickstart on KinD](#quickstart-on-kind)
- [Install on a real cluster](#install-on-a-real-cluster)
- [Security model](#security-model)
- [Operational concerns & known sharp edges](#operational-concerns--known-sharp-edges)
- [Design notes](#design-notes)
- [Roadmap](#roadmap)
- [License](#license)

## What is nfsZ?

Kubernetes has no native way to share a live, multi-writer volume across
namespaces: PVCs are namespaced and a PV binds exactly one PVC. The usual
answer is "stand up an NFS server somewhere and hand-craft PV/PVC pairs."
nfsZ collapses all of that into one resource:

```yaml
apiVersion: nfsz.dev/v1alpha1
kind: SharedVolume
metadata:
  name: shared-data
spec:
  capacity: 10Gi
  namespaces:
    names: [team-a, team-b]          # explicit targets…
    selector:                        # …and/or select namespaces by label
      matchLabels:
        nfsz.dev/shared-data: "true"
```

Pods in every target namespace then just mount the PVC named after the
SharedVolume:

```yaml
volumes:
  - name: shared
    persistentVolumeClaim:
      claimName: shared-data
```

Writes in one namespace are immediately visible in the others. `kubectl get
sharedvolumes` (or `kubectl get sv`) shows the rollup:

```
NAME          PHASE   CAPACITY   SERVER               BOUND   AGE
shared-data   Ready   10Gi       10.96.218.219:2049   2/2     31s
```

## Is nfsZ right for you?

### ✅ Good fits

- **Home labs and dev/CI clusters** where every workload is trusted.
- **ML models and artifacts** — write once (or rarely), read from many
  namespaces.
- **Media libraries** served by apps living in different namespaces.
- **Build and dependency caches** shared across team namespaces.
- **Shared config / reference data** that many namespaces read and a few
  write.
- **Cross-namespace scratch space** — log aggregation, handoff directories,
  batch pipelines.

### ❌ Do NOT use it for

- **Databases. Any of them.** Postgres, MySQL, etcd — and SQLite-heavy apps
  (heavy byte-range locking over NFS is a corruption story waiting to be
  told).
- **Multi-tenant clusters with untrusted workloads** — see the
  [security model](#security-model); the per-volume NetworkPolicy helps
  only where the CNI enforces it, and AUTH_SYS remains spoofable inside
  target namespaces.
- **Anything that needs enforced quotas** — capacity is advisory (see
  [sharp edges](#operational-concerns--known-sharp-edges)).
- **IOPS- or latency-sensitive workloads** — userspace NFS over a Service
  hop is built for convenience, not speed.
- **Data you can't afford to lose** without an external backup — durability
  is exactly your backing StorageClass's durability, nothing more.
- **Production.** See the notice at the top.

## How it works

For each SharedVolume the operator provisions, entirely inside the cluster:

1. a **backing PVC** (any StorageClass; defaults to the cluster default) that
   holds the actual data;
2. a single-replica **NFS-Ganesha server** pod — a *userspace* NFSv4.1/4.2
   server, so no kernel `nfsd`, no privileged mode (just
   `DAC_READ_SEARCH`/`SYS_RESOURCE`), and no rpcbind/mountd/statd: one TCP
   port (2049);
3. a ClusterIP **Service** for the server;
4. per target namespace, a **pre-bound PV + PVC pair** pointing at the
   server. Namespaces matched by the label selector are picked up (and
   garbage-collected) automatically as labels change.

Deletion is ordered (consumer PVCs → PVs → server → backing PVC) via a
finalizer. `spec.reclaimPolicy: Retain` keeps the backing PVC and your data.

## Quickstart on KinD

Everything runs in KinD, including on macOS (verified on Docker Desktop,
Apple Silicon). The stock `kindest/node` image lacks `nfs-common`, so use
either `make kind-up` (custom node image) or `make kind-prep` (retrofits a
running cluster).

From zero to verified cross-namespace sharing:

```sh
make kind-up                          # NFS-capable kind cluster
make dev-deploy IMG=nfsz/manager:dev  # build + load images, deploy operator
make demo                             # SharedVolume across two namespaces + live write/read check
make demo-clean                       # remove the example
make kind-down                        # delete the cluster (detaches NFS mounts first)
```

`make demo` applies [examples/quickstart.yaml](examples/quickstart.yaml): a
1Gi SharedVolume targeting `demo-a` by name and `demo-b` by label, a writer
pod appending timestamps in `demo-a`, and a reader tailing the same file
from `demo-b`.

Tests:

```sh
make test           # envtest unit tests
make test-e2e       # full lifecycle against kind (cross-ns I/O, server-kill recovery, teardown)
make phase0         # standalone NFSv4-in-kind validation spike (no operator)
```

## Backup & restore

Without backups, a SharedVolume's data lives inside the kind node container
and dies with `kind delete cluster`. `spec.backup` fixes that: a per-volume
CronJob rsyncs the data to a directory that exists on every node, and on
kind that directory is a **host mount** — `make kind-up` maps
`$NFSZ_BACKUP_HOST_DIR` (default `~/.nfsz/backups`) into every node at
`/var/nfsz-backups`, so the mirror survives anything that happens to the
cluster:

```yaml
spec:
  backup:
    schedule: "0 */6 * * *"
    destination:
      hostPath:
        path: /var/nfsz-backups   # the *node* path; kind maps it to the host dir
```

See [examples/backup.yaml](examples/backup.yaml). The mirror at
`~/.nfsz/backups/<volume>/` is a **plain browsable file tree** — media files
are directly playable from the host, no restore tooling needed. Run a backup
immediately with
`kubectl -n nfsz-system create job --from=cronjob/nfsz-<volume>-backup backup-now`;
`status.lastBackupTime` tracks the last success.

**Restore is automatic.** When a SharedVolume with `spec.backup` is created
and a mirror for it already exists at the destination, a one-shot seed job
copies it into the fresh volume *before* the NFS server first starts (the
`Restored` condition reports progress). Disaster recovery is therefore:

```sh
make kind-up && make dev-deploy IMG=nfsz/manager:dev
kubectl apply -f <your SharedVolume manifests>   # data comes back by itself
```

The seed never overwrites anything: it runs only before the server's first
start, and refuses to touch a volume that already contains data. Opt out
with `restorePolicy: Never`.

Caveats, deliberately accepted for simplicity:

- **It's a mirror, not versioned backup.** `rsync --delete` propagates
  deletions on the next run — an accidental `rm` in the cluster reaches the
  mirror at the next schedule tick. Point Time Machine (or anything) at the
  host dir if you want history.
- **Per-file consistency only.** A backup during heavy writes can capture a
  torn view across files. Fine for media libraries; wrong for databases.
- **Backup pods use hostPath and run as root** (rsync must read every UID's
  files). Namespaces enforcing `baseline`/`restricted` Pod Security
  Admission will reject them; `nfsz-system` ships without PSA labels.
- Clusters created before this feature lack the node mount — `make
  kind-down kind-up` to pick it up.

## Install on a real cluster

```sh
kubectl apply -f dist/install.yaml
```

or with Helm:

```sh
helm install nfsz dist/chart -n nfsz-system --create-namespace
```

Requirements on cluster nodes: the NFS *client* userland (`mount.nfs`, i.e.
`nfs-common`/`nfs-utils`) and kernel NFS client support. Virtually every
managed/distro Kubernetes node has both; immutable minimal OSes (e.g.
Talos) need it configured. Read the next two sections before installing
anywhere that matters.

> ⚠️ **Uninstall order matters.** Delete all SharedVolumes and let them
> finish finalizing **before** removing the operator
> (`kubectl delete -f dist/install.yaml` / `helm uninstall`). Removing the
> controller first strands CRs whose finalizers can no longer run — CRD
> deletion then hangs forever, and NFS servers die under live mounts (see
> the wedge warning below). `make undeploy` drains SharedVolumes for you.

## Security model

NFS with `sec=sys` (AUTH_SYS) means the *client asserts its own UID*, and
nfsZ exports with `No_Root_Squash` — so reachability equals full
root-equivalent access to a share. A pod doesn't even need mount privileges
to speak NFS: a userspace client library (e.g. libnfs) talks plain TCP from
an unprivileged container. nfsZ's defense is network-level:

**Per-volume NetworkPolicy (default: enabled).** Each SharedVolume gets a
NetworkPolicy on its server pod admitting TCP 2049 only from:

- **cluster nodes** (auto-discovered, refreshed as nodes join/leave) —
  kubelet performs the real mounts from the node's network namespace, so
  node IPs are the legitimate traffic source;
- **pods in target namespaces** — matched via the
  `kubernetes.io/metadata.name` label;
- any extra CIDRs passed via `--allow-cidrs` (see caveats).

Everything else — i.e. a rogue pod in a non-target namespace — is denied.
Opt out per volume with `spec.networkPolicy: Disabled`.

> ⚠️ **NetworkPolicy is only as real as your CNI.** Policies are enforced
> by the CNI plugin; **kind's default CNI (kindnet) does not enforce them
> at all**, and on any cluster without a policy-capable CNI (Calico,
> Cilium, etc.) the policy object exists but does nothing. Also note the
> node-IP allowance means hostNetwork pods are admitted, and CNIs that SNAT
> cross-node traffic to tunnel IPs (e.g. Calico in VXLAN mode) need those
> tunnel ranges passed via `--allow-cidrs`, which widens the allowance to
> that CIDR. Treat the policy as a strong lock on a door whose frame is
> your CNI.

What you get beyond the policy:

- **Admin-gated creation**: SharedVolume is cluster-scoped, so only cluster
  admins decide what is shared and where it appears.
- **In-cluster only**: the server is a ClusterIP Service — nothing is
  exposed outside the cluster.
- **Unprivileged server**: ganesha runs without privileged mode (only
  `DAC_READ_SEARCH` + `SYS_RESOURCE`), as a userspace process.
- Traffic is unencrypted NFS inside the cluster network; Kerberos
  (`sec=krb5p`) would fix both authentication and encryption and is a
  distant aspiration.

## Operational concerns & known sharp edges

> ⚠️ **Capacity is advisory, not enforced.** `spec.capacity` sizes the
> backing PVC; the export itself has no quota. Every consumer PVC may say
> `10Gi`, and any single namespace can still fill 100% of the volume for
> everyone. Real enforcement needs filesystem project quotas (roadmap).

> ⚠️ **The Service ClusterIP is load-bearing and immutable.** Generated PVs
> embed the server's ClusterIP forever (NFS mounts resolve on the node,
> where cluster DNS doesn't). If anything recreates that Service — cluster
> restore, GitOps prune-and-reapply, manual deletion — every PV for that
> volume breaks. Data stays safe on the backing PVC, but recovery means
> recreating the PV/PVC pairs and restarting consumer pods. The operator
> never recreates a Service on its own for exactly this reason.

> ⚠️ **Never destroy a server while clients hold mounts.** A `hard` NFS
> mount whose server is *permanently* gone blocks in an uninterruptible
> kernel wait (D-state, immune to SIGKILL). Pods wedge, kubelet can't
> unmount, and on kind the node container becomes unkillable (Docker
> Desktop VM restart required — ask us how we know; it's why `make
> kind-down` force-detaches all NFS mounts before deleting the cluster).
> Always delete the SharedVolume and let the finalizer order the teardown;
> never delete `nfsz-system` or the server Deployment out from under live
> consumers.

The rest, roughly by likelihood of biting you:

- **Single-replica server per volume.** A pod restart pauses I/O for about
  the 15s grace period — clients block and recover automatically (tested).
  A *node* failure stalls I/O until the RWO backing PVC force-detaches and
  the pod reschedules, which on many storage backends takes minutes.
- **Durability = your backing StorageClass.** nfsZ adds no replication. On
  local-path in a home lab, the data lives on one disk of one node. Use
  `spec.backup` (see [Backup & restore](#backup--restore)) to mirror the
  data off-cluster; Velero remains confused by the pre-bound PVs, so don't
  rely on it.
- **kube-proxy assumption.** Mounts originate in the node's network
  namespace, so ClusterIPs must be reachable from there. Standard
  kube-proxy (iptables/IPVS): yes. Cilium with kube-proxy replacement:
  usually works via host socket LB — verify before relying on it.
- **NFS semantics, not POSIX.** Close-to-open consistency; concurrent
  appends from different namespaces interleave fine, but apps doing
  concurrent random writes to the same file need NFSv4 byte-range locking
  and testing.
- **Everyone is root on the share.** `No_Root_Squash` and a `1777` export
  root keep things simple; per-volume squash/uid options are on the
  roadmap.
- **`spec.capacity` is immutable** — no expansion yet; plan sizes up front.

## Design notes

- **NFSv4.x only.** v3 needs rpcbind/statd/lockd, which are exactly the
  things that break in containers. v4.1+ is one TCP port with built-in
  locking; ganesha's recovery state lives on the backing PVC so clients
  reclaim locks across server restarts.
- **ClusterIP, not DNS, in PVs.** The in-tree NFS mount resolves the server
  name *on the node*, where cluster DNS doesn't resolve (notably in kind).
- **`storageClassName: ""` + pre-binding.** Generated PVs/PVCs pin to each
  other (`claimRef` + `volumeName`) and opt out of dynamic provisioning, so
  the default StorageClass can't capture them. Released PVs are repaired
  automatically if a consumer PVC is deleted and recreated.
- **Conflicts are surfaced, never adopted.** If a target namespace already
  has a PVC with the SharedVolume's name, the binding reports `Conflict`
  (and the SharedVolume goes `Degraded`) rather than touching it.

## Roadmap

Shipped in v1:

- [x] `SharedVolume` CRD — explicit namespace list + label selector (union)
- [x] In-cluster, unprivileged, userspace NFSv4.1/4.2 server per volume
- [x] Pre-bound PV/PVC fan-out; newly labeled namespaces auto-provision
- [x] Garbage collection when namespaces leave the target set
- [x] Ordered finalizer teardown + `reclaimPolicy: Retain`
- [x] Released-PV repair (delete + recreate a consumer PVC and it rebinds)
- [x] Conflict surfacing — foreign PVCs are reported, never adopted
- [x] Status: phases, conditions, per-namespace bindings, printer columns
- [x] One-command install (`dist/install.yaml`) + Helm chart (`dist/chart`)
- [x] NFS-capable KinD tooling, envtest + e2e suites, quickstart demo
- [x] Wedge-proof `make kind-down` (force-detaches NFS mounts pre-delete)
- [x] **Per-volume NetworkPolicy** (default on; `spec.networkPolicy` to opt
      out) admitting only cluster nodes + target namespaces on 2049 —
      closes the cluster-wide access hole *where the CNI enforces policy*;
      `--allow-cidrs` for tunnel-SNAT CNIs
- [x] **Scheduled backups + auto-restore** (`spec.backup`) — per-volume
      rsync mirror to a host-mounted directory that survives cluster
      deletion; fresh volumes auto-seed from an existing mirror before the
      server first starts

Planned — security first:

- [ ] Export options: squash/anon-uid, per-namespace read-only
- [ ] Namespace opt-in/grant model (consuming namespaces must consent)
- [ ] Quota enforcement (XFS project quotas on the backing filesystem)
- [ ] Metrics + events/alerts for `Degraded` (no silent failures)
- [ ] Capacity expansion (mutable `spec.capacity`)
- [ ] HA / failover story for the server
- [ ] DNS-based endpoints for clusters where node-level DNS works
- [ ] CSI backend option (`--pv-backend=csi` via csi-driver-nfs)
- [ ] More backup destinations (S3-compatible) + versioned engines (restic)
- [ ] Kerberos (`sec=krb5p`) — real authn + encryption; aspirational

## License

Apache-2.0
