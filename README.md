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

## Install

```sh
kubectl apply -f dist/install.yaml
```

or with Helm:

```sh
helm install nfsz dist/chart -n nfsz-system --create-namespace
```

Requirements on cluster nodes: the NFS *client* userland (`mount.nfs`, i.e.
`nfs-common`/`nfs-utils`) and kernel NFS client support. Virtually every
managed/distro Kubernetes node has both. **KinD nodes do not** — see below.

## KinD / local development (macOS-friendly)

Everything runs in KinD, including on macOS (verified on Docker Desktop,
Apple Silicon). The stock `kindest/node` image lacks `nfs-common`, so use
either:

```sh
make kind-up        # builds an NFS-capable node image and creates the cluster
# …or for an existing cluster:
make kind-prep      # docker-execs nfs-common into the running nodes
```

Quickstart (from zero to verified cross-namespace sharing):

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

## Design notes

- **NFSv4.x only.** v3 needs rpcbind/statd/lockd, which are exactly the
  things that break in containers. v4.1+ is one TCP port with built-in
  locking; ganesha's recovery state lives on the backing PVC so clients
  reclaim locks across server restarts.
- **ClusterIP, not DNS, in PVs.** The in-tree NFS mount resolves the server
  name *on the node*, where cluster DNS doesn't resolve (notably in kind).
  The operator therefore never recreates a Service while its PVs exist.
- **`storageClassName: ""` + pre-binding.** Generated PVs/PVCs pin to each
  other (`claimRef` + `volumeName`) and opt out of dynamic provisioning, so
  the default StorageClass can't capture them. Released PVs are repaired
  automatically if a consumer PVC is deleted and recreated.
- **Conflicts are surfaced, never adopted.** If a target namespace already
  has a PVC with the SharedVolume's name, the binding reports `Conflict`
  (and the SharedVolume goes `Degraded`) rather than touching it.

## Limitations (v1)

- The per-volume NFS server is a single replica: a restart pauses I/O for
  roughly the grace period (~15s) while `hard`-mounted clients block and
  recover. No HA yet.
- `spec.capacity` is immutable (no expansion yet).
- All consumers get root-equivalent access to the share (`No_Root_Squash`);
  per-volume squash/uid options are on the roadmap.
- Tenancy is admin-mediated: SharedVolume is cluster-scoped, so only cluster
  admins decide what is shared where. A namespace opt-in/grant model is
  planned.

## Roadmap

Namespace opt-in/grant model, CSI backend option, DNS-based endpoints,
export options (squash, read-only namespaces), capacity expansion, metrics,
HA server.

## License

Apache-2.0
