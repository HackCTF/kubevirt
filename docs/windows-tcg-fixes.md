# Windows 10 TCG Fixes for KubeVirt v1.3.1

This document describes 5 fixes applied to KubeVirt v1.3.1 to enable Windows 10
VirtualMachineInstances under TCG (software emulation, no KVM).

**Branch:** [`hackctf/win10-tcg-fixes`](https://github.com/HackCTF/kubevirt/tree/hackctf/win10-tcg-fixes)
**Based on:** `v1.3.1` tag
**Status:** Verified on production cluster (Combate) — `virt-launcher:fixed-v5`

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

### Fix 4: Single-pass PCI topology (removes root cause entirely)

**File:** `pkg/virt-launcher/virtwrap/nichotplug.go:163`

Instead of the two-pass approach (placeholder interfaces → define →
read-back → strip placeholders → re-define), the new code directly adds
`pcie-root-port` controllers to the domain spec before the single
`virDomainDefineXML` call.

**Before:** ~65 lines with `appendPlaceholderInterfacesToTheDomain`,
`newInterfacePlaceholder`, and the two-pass logic in `withNetworkIfacesResources`.

**After:** ~15 lines:

```go
func withNetworkIfacesResources(vmi *v1.VirtualMachineInstance, domainSpec *api.DomainSpec, f func(v *v1.VirtualMachineInstance, s *api.DomainSpec) (cli.VirDomain, error)) (cli.VirDomain, error) {
    if len(vmi.Spec.Domain.Devices.Interfaces) == 0 {
        return f(vmi, domainSpec)
    }
    if val := vmi.Annotations[v1.PlacePCIDevicesOnRootComplex]; val == "true" {
        return f(vmi, domainSpec)
    }
    reservedSlots := ReservedInterfaces - len(vmi.Spec.Domain.Devices.Interfaces)
    for i := 0; i < reservedSlots; i++ {
        domainSpec.Devices.Controllers = append(domainSpec.Devices.Controllers, api.Controller{
            Type:  "pci",
            Index: fmt.Sprintf("%d", i+1),
            Model: "pcie-root-port",
        })
    }
    return f(vmi, domainSpec)
}
```

**Dependency removed:** Fix 3 (UUID preservation) is no longer needed —
there is only one `virDomainDefineXML` call, so UUID is generated once
and never mismatched.

**Commit:** [`9000317`](https://github.com/HackCTF/kubevirt/commit/9000317)

### Fix 4a: Controller index attribute

**File:** `pkg/virt-launcher/virtwrap/nichotplug.go:172`

The initial Fix 4 implementation omitted the `Index` attribute on PCI
controllers. libvirt requires a unique integer index for each controller:

```
XML error: Invalid value for attribute 'index' in element 'controller':
''. Expected integer value
```

**Fix:** Add `Index: fmt.Sprintf("%d", i+1)` to the controller struct.

**Commit:** [`d304acc`](https://github.com/HackCTF/kubevirt/commit/d304acc)

## Fix Summary

| Bug | File | Change | Lines | Severity |
|-----|------|--------|-------|----------|
| 1 | `manager.go` | nil guard before `nic.Alias.GetName()` | +3 | P0 - crash |
| 2 | `schema.go` | pointer receiver + nil check in `GetName()`/`IsUserDefined()` | +8/-2 | P0 - crash |
| 3 | `nichotplug.go` | preserve UUID across double define | +1 | P0 - root cause |
| 4 | `nichotplug.go` | single-pass PCI topology (remove two-pass) | +9/-65 | P1 - perf |
| 4a | `nichotplug.go` | add controller Index attribute | +1 | P1 - libvirt compat |

Total: **22 insertions, 67 deletions** across 3 files.

## Build

Prerequisites: Go 1.22+, `libvirt-dev`, Docker, access to a container registry.

### Image tags

| Tag | Fixes | Use |
|-----|-------|-----|
| `virt-launcher:fixed` | P0 (1+2+3) | Original fix |
| `virt-launcher:fixed-v4` | P0 + Fix 4 (missing Index) | Broken, do not use |
| `virt-launcher:fixed-v5` | P0 + Fix 4 + Fix 4a | **Current stable** |

```bash
# Clone the fork
git clone -b hackctf/win10-tcg-fixes https://github.com/HackCTF/kubevirt.git
cd kubevirt

# Build the virt-launcher binary
CGO_ENABLED=1 go build -o virt-launcher ./cmd/virt-launcher/

# Build Docker image (using official base for libvirt deps)
docker build -t registry.example.org/library/virt-launcher:fixed-v5 -f- . <<'DOCKERFILE'
FROM quay.io/kubevirt/virt-launcher:v1.3.1
COPY --chmod=755 virt-launcher /usr/bin/virt-launcher
DOCKERFILE

# Push to registry
docker push registry.example.org/library/virt-launcher:fixed-v5
```

### Deploy

```bash
# Point virt-controller to the custom image
kubectl patch deploy -n kubevirt virt-controller --type merge -p '{
  "spec":{"template":{"spec":{"containers":[{
    "name":"virt-controller",
    "args":["--launcher-image","registry.example.org/library/virt-launcher:fixed-v5",
             "--exporter-image","quay.io/kubevirt/virt-exportserver:v1.3.1",
             "--port","8443","-v","2"]
  }]}}}}
}'

# Wait for rollout
kubectl rollout status deployment virt-controller -n kubevirt

# Delete old virt-launcher pods to pick up new image
kubectl delete pod -n win-lab -l kubevirt.io=virt-launcher
```

### Verify launcher image

```bash
kubectl get deploy -n kubevirt virt-controller -o jsonpath='{.spec.template.spec.containers[0].args}'
# Expected: ["--launcher-image","registry.example.org/library/virt-launcher:fixed-v5",...]
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

### Fix 4 verification: VMI creation latency

With Fix 4, VMI creation is single-pass — only one `virDomainDefineXML` call.
Observed transition times from the cluster:

```
win10-tcg-test-7:
  Pending:    15:29:00
  Scheduling: 15:29:00
  Scheduled:  15:29:06
  Running:    15:29:07  ← ~7s from Pending to Running
```

### Fix 4a verification: Controller XML

Before fix:
```xml
<controller type="pci" index="" model="pcie-root-port"/>
```

After fix (libvirt-compliant):
```xml
<controller type="pci" index="1" model="pcie-root-port"/>
<controller type="pci" index="2" model="pcie-root-port"/>
<controller type="pci" index="3" model="pcie-root-port"/>
```

### QEMU command line (TCG + PCI topology)

```
-accel tcg
-machine pc-q35-rhel9.4.0,...,acpi=off
-device {"driver":"pcie-root-port","port":16,"chassis":1,"id":"pci.1","bus":"pcie.0"}
-device {"driver":"pcie-root-port","port":17,"chassis":2,"id":"pci.2","bus":"pcie.0"}
-device {"driver":"pcie-root-port","port":18,"chassis":3,"id":"pci.3","bus":"pcie.0"}
...
-m size=6291456k
```

### OOMKill threshold

QEMU under TCG requires significant memory overhead beyond guest RAM:

| VMI | Guest | Container limit | Result | Survival |
|-----|-------|-----------------|--------|----------|
| test-5 | 4Gi | 4.3Gi | OOMKilled | ~7min |
| test-6 | 6Gi | 6.3Gi | OOMKilled | ~3.5min |
| test-7 | 6Gi | 8.3Gi | **Stable** | **7.5min+** |

**Rule:** For Windows 10 under TCG, set container memory limit to
`guest_RAM + 2Gi` (approximately 30% overhead for QEMU + TCG).

## Remaining Issues

1. **TCG performance:** Software emulation is extremely slow for Windows
   (~2-5% native). Enable KVM (nested virtualization) for production use.
   The 5 fixes make VMI creation and runtime stable — performance is the
   remaining limitation.

2. **Node memory pressure:** 8Gi container limit for 6Gi guest RAM reduces
   VMI density on small nodes. Use TCG only for testing; production should
   use KVM.
