# kindest/node with NFS client userland (mount.nfs) baked in.
# The stock image lacks nfs-common, so kubelet cannot exec mount.nfs
# for in-tree nfs volumes (kind issue #1806).
ARG BASE=kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5
FROM ${BASE}
RUN apt-get update && apt-get install -y --no-install-recommends nfs-common \
    && rm -rf /var/lib/apt/lists/*
