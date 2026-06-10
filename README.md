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
  [security model](#security-model); the namespace boundary is *not* an
  access boundary today.
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

## Security model

> ⚠️ **MAJOR CONCERN — target namespaces are a *provisioning* boundary, not
> an *access* boundary.** The NFS Service is a plain ClusterIP serving
> `sec=sys` (AUTH_SYS — the *client asserts its own UID*) with
> `No_Root_Squash`. **Any pod in the cluster** can talk to
> `<clusterIP>:2049` and read/write every share **as any UID, including
> root**. It doesn't even need mount privileges: a userspace NFS client
> library (e.g. libnfs) speaks the protocol over plain TCP from an
> unprivileged container. Until per-volume NetworkPolicies land (top
> roadmap item), only run nfsZ where **every workload in the cluster is
> trusted**.

What you do get today:

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
  local-path in a home lab, the data lives on one disk of one node. There
  is no snapshot or backup integration, and backup tools like Velero will
  be confused by the pre-bound PVs — back up the *backing PVC's* contents
  externally.
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

Planned — security first:

- [ ] **Per-volume NetworkPolicy** restricting port 2049 to target
      namespaces — *top priority; closes the cluster-wide access hole on
      CNIs that enforce policy*
- [ ] Export options: squash/anon-uid, per-namespace read-only
- [ ] Namespace opt-in/grant model (consuming namespaces must consent)
- [ ] Quota enforcement (XFS project quotas on the backing filesystem)
- [ ] Metrics + events/alerts for `Degraded` (no silent failures)
- [ ] Capacity expansion (mutable `spec.capacity`)
- [ ] HA / failover story for the server
- [ ] DNS-based endpoints for clusters where node-level DNS works
- [ ] CSI backend option (`--pv-backend=csi` via csi-driver-nfs)
- [ ] Snapshot/backup integration + Velero guidance
- [ ] Kerberos (`sec=krb5p`) — real authn + encryption; aspirational

## License

Apache-2.0
