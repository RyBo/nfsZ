# Templated by phase0.sh: ${SERVER_IP} is the ClusterIP of nfsz-server.
apiVersion: v1
kind: Namespace
metadata:
  name: ns-a
---
apiVersion: v1
kind: Namespace
metadata:
  name: ns-b
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: nfsz-shared-ns-a
spec:
  capacity:
    storage: 1Gi
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  mountOptions: [nfsvers=4.1, proto=tcp, hard]
  nfs:
    server: ${SERVER_IP}
    path: /
  claimRef:
    apiVersion: v1
    kind: PersistentVolumeClaim
    namespace: ns-a
    name: shared
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: nfsz-shared-ns-b
spec:
  capacity:
    storage: 1Gi
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  mountOptions: [nfsvers=4.1, proto=tcp, hard]
  nfs:
    server: ${SERVER_IP}
    path: /
  claimRef:
    apiVersion: v1
    kind: PersistentVolumeClaim
    namespace: ns-b
    name: shared
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: shared
  namespace: ns-a
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  volumeName: nfsz-shared-ns-a
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: shared
  namespace: ns-b
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  volumeName: nfsz-shared-ns-b
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: writer
  namespace: ns-a
spec:
  terminationGracePeriodSeconds: 1
  containers:
    - name: writer
      image: busybox:1.36
      command: ["sh", "-c", "while true; do date >> /mnt/log || exit 1; sleep 1; done"]
      volumeMounts:
        - name: shared
          mountPath: /mnt
  volumes:
    - name: shared
      persistentVolumeClaim:
        claimName: shared
---
apiVersion: v1
kind: Pod
metadata:
  name: reader
  namespace: ns-b
spec:
  terminationGracePeriodSeconds: 1
  containers:
    - name: reader
      image: busybox:1.36
      command: ["sh", "-c", "touch /mnt/log; tail -f /mnt/log"]
      volumeMounts:
        - name: shared
          mountPath: /mnt
  volumes:
    - name: shared
      persistentVolumeClaim:
        claimName: shared
