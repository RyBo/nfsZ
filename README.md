```text
         __        _____
  _ __  / _| ___  |__  /
 | '_ \| |_ / __|   / /
 | | | |  _|\__ \  / /_
 |_| |_|_|  |___/ /____|
```

**Cross-namespace RWX volumes for Kubernetes, with no external NFS server.**

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
- [Backup & restore](#backup--restore)
- [Install on a real cluster](#install-on-a-real-cluster)
- [Security model](#security-model)
- [Operational concerns & known sharp edges](#operational-concerns--known-sharp-edges)
- [Design notes](#design-notes)
- [Roadmap](#roadmap)
- [License](#license)

## What is nfsZ?

Kubernetes has no native way to share a live, multi-writer volume across
namespaces. PVCs are namespaced and a PV binds exactly one PVC, so the
usual advice is to stand up an NFS server somewhere and hand-craft PV/PVC
pairs against it. nfsZ collapses all of that into one resource:

```yaml
apiVersion: nfsz.dev/v1alpha1
kind: SharedVolume
metadata:
  name: shared-data
spec:
  capacity: 10Gi
  namespaces:
    names: [team-a, team-b]          # explicit targets
    selector:                        # and/or select namespaces by label
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

Good fits:

- Home labs and dev/CI clusters where every workload is trusted.
- ML models and artifacts that are written once and read from many
  namespaces.
- Media libraries served by apps living in different namespaces.
- Build and dependency caches shared across team namespaces.
- Shared config or reference data that many namespaces read and a few
  write.
- Cross-namespace scratch space: log aggregation, handoff directories,
  batch pipelines.

Bad fits:

- Databases. Any of them. Postgres, MySQL, etcd, and SQLite-heavy apps all
  do heavy byte-range locking, and over NFS that is a corruption story
  waiting to be told.
- Multi-tenant clusters with untrusted workloads. See the
  [security model](#security-model): the per-volume NetworkPolicy helps
  only where the CNI enforces it, and AUTH_SYS remains spoofable inside
  target namespaces.
- Anything that needs enforced quotas. Capacity is advisory (see
  [sharp edges](#operational-concerns--known-sharp-edges)).
- IOPS- or latency-sensitive workloads. Every request crosses a Service
  hop into a userspace NFS server. Convenient, yes. Fast, no.
- Irreplaceable data without a backup configured. Durability is whatever
  your backing StorageClass provides, so at minimum set up
  [`spec.backup`](#backup--restore).
- Production. See the notice at the top.

## How it works

For each SharedVolume the operator provisions, entirely inside the cluster:

1. a backing PVC (any StorageClass, defaults to the cluster default) that
   holds the actual data;
2. a single-replica NFS-Ganesha server pod. Ganesha is a userspace
   NFSv4.1/4.2 server, so there is no kernel `nfsd`, no privileged mode
   (just `DAC_READ_SEARCH` and `SYS_RESOURCE`), and no rpcbind/mountd/statd.
   The whole thing is one TCP port: 2049;
3. a ClusterIP Service for the server;
4. per target namespace, a pre-bound PV and PVC pair pointing at the
   server. Namespaces matched by the label selector are picked up (and
   garbage-collected) automatically as labels change.

Deletion runs through a finalizer in a fixed order: consumer PVCs first,
then PVs, then the server, then the backing PVC. Set
`spec.reclaimPolicy: Retain` if you want the backing PVC and your data to
outlive the SharedVolume.

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
and dies with `kind delete cluster`. `spec.backup` fixes that. A per-volume
CronJob rsyncs the data to a directory that exists on every node, and on
kind that directory is a host mount: `make kind-up` maps
`$NFSZ_BACKUP_HOST_DIR` (default `~/.nfsz/backups`) into every node at
`/var/nfsz-backups`, so the mirror survives anything that happens to the
cluster.

```yaml
spec:
  backup:
    schedule: "0 */6 * * *"
    destination:
      hostPath:
        path: /var/nfsz-backups   # the node path; kind maps it to the host dir
```

See [examples/backup.yaml](examples/backup.yaml). The mirror at
`~/.nfsz/backups/<volume>/` is a plain file tree you can browse. Media
files play straight off the host with no restore tooling in between. To run
a backup right now instead of waiting for the schedule:

```sh
kubectl -n nfsz-system create job --from=cronjob/nfsz-<volume>-backup backup-now
```

`status.lastBackupTime` on the SharedVolume tracks the last success.

Restore is automatic. When a SharedVolume with `spec.backup` is created and
a mirror for it already exists at the destination, a one-shot seed job
copies the mirror into the fresh volume before the NFS server starts for
the first time (the `Restored` condition reports progress). Disaster
recovery is therefore:

```sh
make kind-up && make dev-deploy IMG=nfsz/manager:dev
kubectl apply -f <your SharedVolume manifests>   # data comes back by itself
```

The seed never overwrites anything. It runs only before the server's first
start, and it refuses to touch a volume that already contains data. Opt out
with `restorePolicy: Never`.

Caveats, deliberately accepted to keep this simple:

- The mirror has no version history. `rsync --delete` propagates deletions,
  so an accidental `rm` in the cluster reaches the mirror at the next
  schedule tick. Point Time Machine (or any snapshotting tool) at the host
  dir if you want history.
- Consistency is per-file. A backup that runs during heavy writes can
  capture a torn view across files. That's fine for a media library and
  wrong for a database, which you shouldn't be putting on nfsZ anyway.
- Backup pods use hostPath and run as root, since rsync has to read files
  owned by every UID. Namespaces enforcing `baseline` or `restricted` Pod
  Security Admission will reject them. `nfsz-system` ships without PSA
  labels.
- Clusters created before this feature lack the node mount. Run
  `make kind-down kind-up` to pick it up.

## Install on a real cluster

```sh
kubectl apply -f dist/install.yaml
```

or with Helm:

```sh
helm install nfsz dist/chart -n nfsz-system --create-namespace
```

Cluster nodes need the NFS client userland (`mount.nfs`, packaged as
`nfs-common` or `nfs-utils`) and kernel NFS client support. Virtually every
managed/distro Kubernetes node has both; immutable minimal OSes like Talos
need it configured. Read the next two sections before installing anywhere
that matters.

> ⚠️ **Uninstall order matters.** Delete all SharedVolumes and let them
> finish finalizing before removing the operator
> (`kubectl delete -f dist/install.yaml` / `helm uninstall`). If the
> controller goes first, the CRs are stranded with finalizers that can no
> longer run, CRD deletion hangs forever, and NFS servers die under live
> mounts (see the wedge warning below). `make undeploy` drains
> SharedVolumes for you.

### Running on k3s

Verified on k3s v1.35, single- and multi-node, with the default flannel and
kube-router stack:

- Install `nfs-common` (`nfs-utils` on RPM distros) on every node. k3s
  doesn't bundle it.
- k3s's default local-path StorageClass works as-is for backing PVCs.
- Unlike kind, k3s actually enforces the per-volume NetworkPolicy through
  its embedded kube-router. Mounts still work on both single- and
  multi-node clusters: flannel masquerades cross-node kubelet traffic to
  the node's flannel.1 tunnel address, and the operator auto-admits each
  node's podCIDR-derived tunnel and bridge addresses precisely for this.
  No `--allow-cidrs` needed.
- Until images are published to a registry, load them with
  `docker save nfsz/... | k3s ctr images import -` on each node.

## Security model

NFS with `sec=sys` (AUTH_SYS) means the client asserts its own UID, and
nfsZ exports with `No_Root_Squash`, so being able to reach a share at all
amounts to full root-equivalent access to it. A pod doesn't even need mount
privileges to speak NFS; a userspace client library like libnfs talks plain
TCP from an unprivileged container. The defense nfsZ gives you is at the
network level.

Each SharedVolume gets a NetworkPolicy on its server pod (default on)
admitting TCP 2049 only from:

- Cluster nodes, auto-discovered and refreshed as nodes join and leave.
  Kubelet performs the real mounts from the node's network namespace, so
  node addresses are the legitimate traffic source. This covers each
  node's InternalIP and ExternalIP plus the first two addresses of its
  podCIDR (the tunnel and bridge addresses, flannel.1 and cni0 on k3s).
  The podCIDR pair matters because masquerading CNIs rewrite
  node-to-remote-pod traffic to the tunnel address, and both addresses
  belong to the node itself; CNIs never hand them out to pods.
- Pods in target namespaces, matched via the `kubernetes.io/metadata.name`
  label.
- Any extra CIDRs passed via `--allow-cidrs` (see caveats).

Everything else is denied, including a rogue pod in a namespace the volume
doesn't target. Opt out per volume with `spec.networkPolicy: Disabled`.

> ⚠️ **NetworkPolicy is only as real as your CNI.** Policies are enforced
> by the CNI plugin. kind's default CNI (kindnet) doesn't enforce them at
> all, and on any cluster without a policy-capable CNI the policy object
> exists but does nothing. Note also that the node allowance admits
> hostNetwork pods, and CNIs whose tunnel IPs come from IPAM rather than
> the podCIDR (Calico in VXLAN mode, for example) still need those tunnel
> ranges passed via `--allow-cidrs`, which widens the allowance to that
> whole CIDR.

Beyond the policy:

- SharedVolume is cluster-scoped, so only cluster admins decide what gets
  shared and where it appears.
- The server sits behind a ClusterIP Service; nothing is exposed outside
  the cluster.
- Ganesha runs as an unprivileged userspace process with only
  `DAC_READ_SEARCH` and `SYS_RESOURCE`.
- Traffic is unencrypted NFS inside the cluster network. Kerberos
  (`sec=krb5p`) would fix both authentication and encryption, and remains a
  distant aspiration.

## Operational concerns & known sharp edges

> ⚠️ **Capacity is advisory.** `spec.capacity` sizes the backing PVC; the
> export itself has no quota. Every consumer PVC may say `10Gi`, and any
> single namespace can still fill 100% of the volume for everyone. Real
> enforcement needs filesystem project quotas (roadmap).

> ⚠️ **The Service ClusterIP is load-bearing and immutable.** Generated PVs
> embed the server's ClusterIP forever (NFS mounts resolve on the node,
> where cluster DNS doesn't). If anything recreates that Service, be it a
> cluster restore, a GitOps prune-and-reapply, or a manual deletion, every
> PV for that volume breaks. Data stays safe on the backing PVC, but
> recovery means recreating the PV/PVC pairs and restarting consumer pods.
> The operator never recreates a Service on its own for exactly this
> reason.

> ⚠️ **Never destroy a server while clients hold mounts.** A `hard` NFS
> mount whose server is permanently gone blocks in an uninterruptible
> kernel wait (D-state, immune to SIGKILL). Pods wedge, kubelet can't
> unmount, and on kind the node container becomes unkillable. Recovering
> takes a Docker Desktop VM restart; ask us how we know. This is why
> `make kind-down` force-detaches all NFS mounts before deleting the
> cluster. Always delete the SharedVolume and let the finalizer order the
> teardown, and never delete `nfsz-system` or the server Deployment out
> from under live consumers.

The rest, roughly by likelihood of biting you:

- One server pod per volume. A pod restart pauses I/O for about the 15s
  grace period; clients block and recover automatically (tested). A node
  failure stalls I/O until the RWO backing PVC force-detaches and the pod
  reschedules, which on many storage backends takes minutes.
- Durability equals your backing StorageClass. nfsZ adds no replication.
  On local-path in a home lab, the data lives on one disk of one node. Use
  [`spec.backup`](#backup--restore) to mirror the data off-cluster. Velero
  gets confused by the pre-bound PVs, so don't rely on it here.
- Mounts originate in the node's network namespace, so ClusterIPs must be
  reachable from there. Standard kube-proxy (iptables/IPVS) is fine.
  Cilium with kube-proxy replacement usually works via its host socket LB,
  but verify before relying on it.
- You get NFS close-to-open consistency, not full POSIX semantics.
  Concurrent appends from different namespaces interleave fine. Apps doing
  concurrent random writes to the same file need NFSv4 byte-range locking
  and testing.
- Everyone is root on the share. `No_Root_Squash` and a `1777` export root
  keep things simple for now; per-volume squash and uid options are on the
  roadmap.
- `spec.capacity` is immutable. There is no expansion yet, so plan sizes
  up front.

## Design notes

- NFSv4.x only. v3 needs rpcbind, statd, and lockd, which are exactly the
  things that break in containers. v4.1+ is one TCP port with built-in
  locking, and ganesha's recovery state lives on the backing PVC so
  clients reclaim locks across server restarts.
- PVs embed the Service ClusterIP rather than a DNS name. The in-tree NFS
  mount resolves the server name on the node, where cluster DNS doesn't
  resolve (notably in kind).
- Generated PVs and PVCs pin to each other through `claimRef` plus
  `volumeName`, with `storageClassName: ""` so dynamic provisioners and
  the default StorageClass can't capture them. Released PVs are repaired
  automatically if a consumer PVC is deleted and recreated.
- Conflicts are surfaced rather than adopted. If a target namespace
  already has a PVC with the SharedVolume's name, the binding reports
  `Conflict` (and the SharedVolume goes `Degraded`) instead of touching
  it.

## Roadmap

Shipped in v1:

- [x] `SharedVolume` CRD with an explicit namespace list and a label
      selector (union of both)
- [x] In-cluster, unprivileged, userspace NFSv4.1/4.2 server per volume
- [x] Pre-bound PV/PVC fan-out; newly labeled namespaces auto-provision
- [x] Garbage collection when namespaces leave the target set
- [x] Ordered finalizer teardown plus `reclaimPolicy: Retain`
- [x] Released-PV repair (delete and recreate a consumer PVC and it
      rebinds)
- [x] Conflict surfacing; foreign PVCs are reported, never adopted
- [x] Status: phases, conditions, per-namespace bindings, printer columns
- [x] One-command install (`dist/install.yaml`) plus Helm chart
      (`dist/chart`)
- [x] NFS-capable KinD tooling, envtest and e2e suites, quickstart demo
- [x] Wedge-proof `make kind-down` (force-detaches NFS mounts pre-delete)
- [x] Per-volume NetworkPolicy, on by default with `spec.networkPolicy` to
      opt out, admitting only cluster nodes and target namespaces on 2049
      where the CNI enforces policy; node podCIDR identities are
      auto-admitted for masquerading CNIs like k3s flannel, and
      `--allow-cidrs` covers the rest
- [x] Scheduled backups and auto-restore (`spec.backup`): per-volume rsync
      mirror to a host-mounted directory that survives cluster deletion,
      with fresh volumes auto-seeded from an existing mirror before the
      server first starts

Planned, security first:

- [ ] Export options: squash/anon-uid, per-namespace read-only
- [ ] Namespace opt-in/grant model (consuming namespaces must consent)
- [ ] Quota enforcement (XFS project quotas on the backing filesystem)
- [ ] Metrics, events, and alerts for `Degraded` so failures aren't silent
- [ ] Capacity expansion (mutable `spec.capacity`)
- [ ] HA / failover story for the server
- [ ] DNS-based endpoints for clusters where node-level DNS works
- [ ] CSI backend option (`--pv-backend=csi` via csi-driver-nfs)
- [ ] More backup destinations (S3-compatible) and versioned engines
      (restic)
- [ ] Kerberos (`sec=krb5p`) for real authentication and encryption;
      aspirational

## License

Apache-2.0
