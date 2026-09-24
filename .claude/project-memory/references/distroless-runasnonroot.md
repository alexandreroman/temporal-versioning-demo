---
name: "Distroless + runAsNonRoot needs numeric runAsUser"
description: "Pod securityContext runAsNonRoot fails on distroless :nonroot unless runAsUser: 65532 is set"
type: project
---

# Distroless + runAsNonRoot needs numeric runAsUser

The backend and worker pods set a hardened pod-level
securityContext with `runAsNonRoot: true`. Both images are
`gcr.io/distroless/static-debian13:nonroot`, whose `USER` is the
non-numeric name `nonroot`. With only `runAsNonRoot: true` the
kubelet refuses to start the container:

  Error: container has runAsNonRoot and image has non-numeric user
  (nonroot), cannot verify user is non-root  → CreateContainerConfigError

**Why:** Kubernetes can only verify non-root from a numeric UID; a
username baked into the image is not enough.

**How to apply:** pin the distroless nonroot UID/GID in the
pod-level securityContext (already done in `k8s/base/backend.yaml` and
`k8s/base/workerdeployment.yaml`):

    securityContext:
      runAsNonRoot: true
      runAsUser: 65532
      runAsGroup: 65532

65532 is the UID/GID of the distroless `nonroot` user; without it the kubelet
rejects the pods (verified empirically). See [[k8s-namespace]].
