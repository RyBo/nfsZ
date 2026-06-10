#!/usr/bin/env bash
# Phase 0 go/no-go spike: prove cross-namespace RWX over NFSv4.1 works in kind
# on this machine. Gates:
#   A: mount.nfs present in the kind node image
#   B: an NFS PVC mount succeeds in a pod (kernel NFS client works in the VM)
#   C: cross-namespace read-your-writes + recovery after ganesha pod restart
set -euo pipefail
cd "$(dirname "$0")"

CLUSTER=nfsz
KCTX=kind-${CLUSTER}
k() { kubectl --context "$KCTX" "$@"; }

step() { printf '\n\033[1;34m== %s ==\033[0m\n' "$*"; }
pass() { printf '\033[1;32mPASS: %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }

step "Build kind node image (nfs-common baked in)"
docker build -t nfsz/kind-node:v1.36.1 -f ../kind/node.Dockerfile ../kind

step "Create kind cluster"
if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --config ../kind/kind-config.yaml --wait 120s
else
  echo "cluster $CLUSTER already exists, reusing"
fi

step "Gate A: mount.nfs present on nodes"
for node in $(kind get nodes --name "$CLUSTER"); do
  docker exec "$node" which mount.nfs >/dev/null || fail "mount.nfs missing on $node"
done
pass "Gate A — mount.nfs present on all nodes"

step "Build + load ganesha image"
docker build -t nfsz/ganesha:dev ../../images/ganesha
kind load docker-image nfsz/ganesha:dev --name "$CLUSTER"

step "Deploy ganesha server stack"
k apply -f 10-server.yaml
k -n nfsz-system rollout status deploy/nfsz-server --timeout=120s

SERVER_IP=$(k -n nfsz-system get svc nfsz-server -o jsonpath='{.spec.clusterIP}')
echo "ganesha ClusterIP: $SERVER_IP"

step "Deploy cross-namespace consumers (PV/PVC pairs + writer/reader pods)"
sed "s/\${SERVER_IP}/$SERVER_IP/g" 20-consumers.yaml.tpl | k apply -f -

step "Gate B: NFS mounts succeed (pods Ready)"
if ! k -n ns-a wait --for=condition=Ready pod/writer --timeout=180s; then
  echo "--- diagnostics ---"
  k -n ns-a describe pod writer | tail -30
  for node in $(kind get nodes --name "$CLUSTER"); do
    echo "--- $node /proc/filesystems (nfs?) ---"
    docker exec "$node" grep -i nfs /proc/filesystems || true
    echo "--- $node dmesg tail ---"
    docker exec "$node" dmesg 2>/dev/null | tail -15 || true
  done
  fail "Gate B — writer pod never became Ready (NFS mount failed)"
fi
k -n ns-b wait --for=condition=Ready pod/reader --timeout=180s
pass "Gate B — NFSv4.1 mounts succeeded in both namespaces"

step "Gate C1: cross-namespace read-your-writes"
sleep 5
WRITES=$(k -n ns-b exec reader -- sh -c 'wc -l < /mnt/log')
[ "${WRITES:-0}" -ge 2 ] || fail "Gate C1 — reader in ns-b sees $WRITES lines from writer in ns-a"
pass "Gate C1 — reader in ns-b sees $WRITES lines written from ns-a"

step "Gate C2: ganesha restart recovery"
BEFORE=$(k -n ns-b exec reader -- sh -c 'wc -l < /mnt/log')
k -n nfsz-system delete pod -l app=nfsz-server --wait=false
k -n nfsz-system rollout status deploy/nfsz-server --timeout=120s
echo "ganesha restarted; waiting for clients to recover (grace period 15s + slack)..."
RECOVERED=0
for i in $(seq 1 24); do
  sleep 5
  AFTER=$(k -n ns-b exec reader -- sh -c 'wc -l < /mnt/log' 2>/dev/null || echo "$BEFORE")
  if [ "$AFTER" -gt "$BEFORE" ]; then RECOVERED=1; break; fi
done
[ "$RECOVERED" = 1 ] || fail "Gate C2 — writes did not resume within 120s of ganesha restart"
pass "Gate C2 — clients recovered after ganesha restart ($BEFORE -> $AFTER lines)"

printf '\n\033[1;32mPhase 0 PASSED — NFSv4.1 cross-namespace RWX works on this machine.\033[0m\n'
