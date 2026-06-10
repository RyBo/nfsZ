# Userspace NFS-Ganesha server (NFSv4-only, VFS FSAL).
# Runs unprivileged with CAP_DAC_READ_SEARCH (+ SYS_RESOURCE); no kernel nfsd needed.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        nfs-ganesha nfs-ganesha-vfs netbase \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sf /proc/self/mounts /etc/mtab
# ganesha.conf is mounted by the operator (or phase0 manifests) as a ConfigMap.
# /var/run/ganesha must exist for the pid file; create it at startup since
# /var/run may be empty at container runtime.
ENTRYPOINT ["/bin/sh", "-c", "mkdir -p /var/run/ganesha && exec /usr/bin/ganesha.nfsd -F -L STDOUT -f /etc/ganesha/ganesha.conf"]
