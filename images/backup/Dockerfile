# rsync runner for SharedVolume backup mirrors and seed-from-backup jobs.
# The operator injects the script via the pod command; this image only needs
# a shell and rsync.
FROM alpine:3.22
RUN apk add --no-cache rsync
