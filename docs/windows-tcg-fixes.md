# Windows 10 TCG Fixes for KubeVirt v1.3.1

This document describes 3 fixes applied to KubeVirt v1.3.1 to enable Windows 10
VirtualMachineInstances under TCG (software emulation, no KVM).

**Branch:** [`hackctf/win10-tcg-fixes`](https://github.com/HackCTF/kubevirt/tree/hackctf/win10-tcg-fixes)
**Based on:** `v1.3.1` tag
**Status:** Verified on production cluster (Combate)

---

## Problem

Windows 10 VMI immediately exits with code 2 (`SIGSEGV` / nil pointer dereference):

```
virt-launcher-win10-tcg-test-2-bssrh   0/2   Error   0   94m
virt-launcher-win10-tcg-test-4-5fcp2   2/2   Running 0   58s  (after fix)
```

Logs show:

```
panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x...]
```

### Reproduction

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstance
metadata:
  name: win10-tcg-test
spec:
  domain:
    cpu:
      cores: 2
    memory:
      guest: 4Gi
    machine:
      type: q35
    devices:
      disks:
      - name: windowshd
        disk:
          bus: virtio
      - name: cloudinitdisk
        disk:
          bus: virtio
      interfaces:
      - name: default
        bridge: {}
  networks:
  - name: default
    pod: {}
  volumes:
  - name: windowshd
    persistentVolumeClaim:
      claimName: golden-win10
  - name: cloudinitdisk
    cloudInitNoCloud:
      userDataBase64: I2Nsb3VkLWNvbmZpZw0KcGFzc3dvcmQ6IFBhc3N3b3JkMTIzITIz
```

## Root Cause Analysis

Three bugs in `virt-launcher` combine to crash on the **second virDomainDefineXML()**
during `withNetworkIfacesResources()`.

### Flow

```
SyncVMI
  └── withNetworkIfacesResources
        ├── [1] Append placeholder interfaces
        ├── [2] virDomainDefineXML (first define)  ← generates UUID
        ├── [3] Read back domain XML from libvirt
        ├── [4] Copy devices back, strip placeholders
        ├── [5] virDomainDefineXML (second define)  ← BUG: different UUID
        │         └── libvirt rejects: "already exists with different UUID"
        └── [6] buildDevicesMetadata reads XML with no <alias>
                  └── BUG: nil Alias → [SIGSEGV at Alias.GetName()]
```

### Bug 1: Missing nil guard in `buildDevicesMetadata`

**File:** `pkg/virt-launcher/virtwrap/manager.go:2020`

When `withNetworkIfacesResources` fails, `buildDevicesMetadata` processes
interfaces from a domain XML that lacks `<alias>` tags. `nic.Alias` is nil.
The code calls `nic.Alias.GetName()` unconditionally.

```go
// BEFORE: crashes when nic.Alias is nil
if data, exist := taggedInterfaces[nic.Alias.GetName()]; exist {
```

**Fix:** Add nil guard before access:

```go
// AFTER: safe
if nic.Alias == nil {
    continue
}
```

**Commit:** [`b6eb452`](https://github.com/HackCTF/kubevirt/commit/b6eb452)

### Bug 2: Value receiver causes SIGSEGV on nil *Alias

**File:** `pkg/virt-launcher/virtwrap/api/schema.go:915-923`

`GetName()` and `IsUserDefined()` use value receivers. When called on a nil
`*Alias`, Go copies the struct to pass by value — but the pointer is nil,
causing SIGSEGV before the method body executes.

```go
// BEFORE: value receiver — SIGSEGV on nil
func (alias Alias) GetName() string {
    return alias.name
}
```

**Fix:** Change to pointer receivers with nil checks:

```go
// AFTER: pointer receiver, nil-safe
func (alias *Alias) GetName() string {
    if alias == nil {
        return ""
    }
    return alias.name
}
```

**Commit:** [`d7b40d2`](https://github.com/HackCTF/kubevirt/commit/d7b40d2)

### Bug 3: UUID mismatch across two-pass domain define

**File:** `pkg/virt-launcher/virtwrap/nichotplug.go:199`

`withNetworkIfacesResources` calls `virDomainDefineXML` **twice**:
1. First with placeholder interfaces (to trigger PCI controller allocation)
2. Then without placeholders (clean config)

Libvirt generates a UUID on the first define. The second define has a
**different UUID** (domainSpec.UUID was never updated after the read-back),
causing libvirt to reject:

```
domain 'X' already exists with uuid Y
```

```go
// BEFORE: UUID from first define is lost
domainSpecWithoutIfacePlaceholders.Devices.DeepCopyInto(&domainSpec.Devices)
// domainSpec.Spec.UUID is still the pre-define value — may differ!
```

**Fix:** Copy the UUID from the read-back spec back to domainSpec:

```go
// AFTER: UUID preserved — libvirt treats second define as upsert
domainSpecWithoutIfacePlaceholders.Devices.DeepCopyInto(&domainSpec.Devices)
domainSpec.UUID = domainSpecWithoutIfacePlaceholders.UUID
```

**Commit:** [`293f0d7`](https://github.com/HackCTF/kubevirt/commit/293f0d7)

### Why Bug 3 triggers Bug 1+2

When the second define fails (Bug 3), `buildDevicesMetadata` reads the
current domain XML. The first define left placeholder interfaces in the
domain, but the second define never completed. `buildDevicesMetadata`
processes interfaces that have no `<alias>` — because the domain is in an
intermediate state with incomplete interface definitions. This triggers
Bug 1 (nil guard) and Bug 2 (pointer receiver) simultaneously.

## Fix Summary

| Bug | File | Change | Lines | Severity |
|-----|------|--------|-------|----------|
| 1 | `manager.go` | nil guard before `nic.Alias.GetName()` | +3 | P0 - crash |
| 2 | `schema.go` | pointer receiver + nil check in `GetName()`/`IsUserDefined()` | +8/-2 | P0 - crash |
| 3 | `nichotplug.go` | preserve UUID across double define | +1 | P0 - root cause |

Total: **12 insertions, 2 deletions** across 3 files.

## Build

Prerequisites: Go 1.22+, `libvirt-dev`, Docker, access to a container registry.

```bash
# Clone the fork
git clone -b hackctf/win10-tcg-fixes https://github.com/HackCTF/kubevirt.git
cd kubevirt

# Build the virt-launcher binary
CGO_ENABLED=1 go build -o virt-launcher ./cmd/virt-launcher/

# Build Docker image (using official base for libvirt deps)
# See Dockerfile.virt-launcher-fixed in patches/
docker build -t registry.example.org/library/virt-launcher:fixed -f- . <<'DOCKERFILE'
FROM quay.io/kubevirt/virt-launcher:v1.3.1
COPY virt-launcher /usr/bin/virt-launcher
DOCKERFILE

# Push to registry
docker push registry.example.org/library/virt-launcher:fixed
```

### Deploy

```bash
# Point virt-controller to the custom image
kubectl patch deploy -n kubevirt virt-controller --type merge -p '{
  "spec":{"template":{"spec":{"containers":[{
    "name":"virt-controller",
    "args":["--launcher-image","registry.example.org/library/virt-launcher:fixed",
             "--exporter-image","quay.io/kubevirt/virt-exportserver:v1.3.1",
             "--port","8443","-v","2"]
  }]}}}}
}'

# Delete old virt-launcher pods to pick up new image
kubectl delete pod -n win-lab -l kubevirt.io=virt-launcher
```

## Verification

### Windows 10 VMI Running

```
$ kubectl get vmi -n win-lab -o wide
NAME               AGE   PHASE     IP          NODENAME         READY
win10-tcg-test-4   58s   Running   10.0.2.82   funny-einstein   True
```

### virt-launcher pod 2/2 Ready, 0 restarts

```
$ kubectl get pods -n win-lab -o wide
NAME                                   READY   STATUS    RESTARTS   AGE
virt-launcher-win10-tcg-test-4-5fcp2   2/2     Running   0          58s
```

Before fix:

```
virt-launcher-win10-tcg-test-2-bssrh   0/2     Error     0          94m
```

### No panic in logs

```
{"component":"virt-launcher","level":"info","msg":"Domain started.",
    "name":"win10-tcg-test-4","namespace":"win-lab","pos":"manager.go:1250"}
{"component":"virt-launcher","level":"info","msg":"Synced vmi",
    "name":"win10-tcg-test-4","namespace":"win-lab","pos":"server.go:208"}
{"component":"virt-launcher","level":"info","msg":"Hardware emulation device
    '/dev/kvm' not present. Using software emulation.","pos":"converter.go:1316"}
```

No `panic`, `SIGSEGV`, `nil pointer`, or `dirty virt-launcher shutdown`.

### TCG confirmed

```
/dev/kvm not present. Using software emulation.
```

## Remaining Issues

1. **Cilium host-to-pod connectivity:** Only one node (`funny-einstein`) has
   working host→pod traffic. Other nodes drop traffic due to
   `kube-proxy-replacement: true` + `routing-mode: tunnel` interaction.
   Pods with health probes (liveness/readiness) must run on that node.

2. **Stale CiliumNode podCIDRs:** Node CIDRs still from old
   `10.0.0.0/8` range after pool migration to `10.200.0.0/16`. Requires
   drain + re-allocation.

3. **TCG performance:** Software emulation is extremely slow for Windows.
   Enable KVM (nested virtualization) for production use.

4. **Fix 4 (P1):** Single-pass PCI topology computation to eliminate the
   two-pass domain define entirely, improving performance and removing
   the root cause scenario entirely.
