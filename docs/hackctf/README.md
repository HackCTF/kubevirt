# HackCTF KubeVirt Fork — Branch `hackctf/win10-tcg-fixes`

## Resumen

Fork de KubeVirt v1.3.1 con **16 commits** que resuelven problemas de estabilidad
bajo TCG (software emulation sin KVM), corren la race condition entre virt-launcher
y multus secondary NADs, y habilitan VMs Windows 10 en el cluster Combate.

**Rama:** `hackctf/win10-tcg-fixes` (basada en `release-1.3`)
**Último commit:** `f38d937f23` — "fix: pre-start hook para TCG + multus secondary NAD race condition"

---

## Tabla de contenidos

1. [Commits en orden cronológico](#commits-en-orden-cronológico)
2. [Fix P0: Crash SIGSEGV — 3 fixes](#fix-p0-crash-sigsegv--3-fixes)
3. [Fix 4: Single-pass PCI topology](#fix-4-single-pass-pci-topology)
4. [Fix 4a: Controller index attribute](#fix-4a-controller-index-attribute)
5. [Netpod readiness watcher (race condition multus)](#netpod-readiness-watcher)
6. [Guest-console-log sidecar fix](#guest-console-log-sidecar-fix)
7. [Pre-start hook TCG + multus](#pre-start-hook-tcg--multus)
8. [Dockerfile](#dockerfile)
9. [Proceso de build](#proceso-de-build)
10. [Despliegue en cluster Kubernetes](#despliegue-en-cluster-kubernetes)
11. [Verificación](#verificación)
12. [Troubleshooting](#troubleshooting)

---

## Commits en orden cronológico

| # | Hash | Descripción | Archivos |
|---|------|-------------|----------|
| 1 | `b6eb452` | fix: nil guard antes de `Alias.GetName()` en `buildDevicesMetadata` | `manager.go` |
| 2 | `d7b40d2` | fix: pointer receiver para `Alias.GetName()`/`IsUserDefined()` | `api/schema.go` |
| 3 | `293f0d7` | fix: preservar UUID entre doble `virDomainDefineXML` | `nichotplug.go` |
| 4 | `1685ad7` | docs: Windows 10 TCG fixes para v1.3.1 | docs |
| 5 | `9000317` | fix: single-pass PCI topology (Fix 4) | `nichotplug.go` |
| 6 | `d304acc` | fix: agregar index a controllers PCI (Fix 4a) | `nichotplug.go` |
| 7 | `891ae96` | docs: Fix 4, Fix 4a, OOMKill threshold | docs |
| 8 | `18d2d09` | docs: design spec para netpod readiness watcher | docs |
| 9 | `0c50614` | docs: implementation plan para netpod readiness watcher | docs |
| 10 | `8c5c7d0` | netpod: esperar interfaces secundarias via netlink RTM_NEWLINK | `netpod.go`, `readiness.go` |
| 11 | `cab2f4a` | build: agregar readiness.go a BUILD.bazel | `BUILD.bazel` |
| 12 | `8b8555b` | netpod: skip waiter when timeout=0; unit tests; opt-in via NetConf | `readiness.go` |
| 13 | `c36219f` | test: integration tests para readiness waiter | tests |
| 14 | `12901ec` | add dockerfile virt-launcher:v1.3.1 | `Dockerfile.hackctf.patch-launcher` |
| 15 | `3e6309c` | fix: guest-console-log sidecar siempre crea log + timeouts non-fatal | `virt-launcher.go`, `virt-tail/main.go` |
| 16 | `f38d937` | fix: pre-start hook para TCG + multus secondary NAD race condition | `Dockerfile`, `virt-launcher.go`, `virt-tail/main.go` |

---

## Fix P0: Crash SIGSEGV — 3 fixes

### Problema

Windows 10 VMI crashea inmediatamente con `SIGSEGV` / nil pointer dereference:

```
panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x...]
```

### Causa raíz

`withNetworkIfacesResources()` ejecuta `virDomainDefineXML` **dos veces**:
1. Con interfaces placeholder (para generar controllers PCI)
2. Sin placeholders (config limpia)

El segundo define falla porque la UUID difiere → `buildDevicesMetadata`
procesa interfaces sin `<alias>` → nil pointer.

### Fix 1: Nil guard en `buildDevicesMetadata`

**Archivo:** `pkg/virt-launcher/virtwrap/manager.go:2020`

```go
// ANTES: crash cuando nic.Alias es nil
if data, exist := taggedInterfaces[nic.Alias.GetName()]; exist {

// DESPUÉS: nil guard
if nic.Alias == nil {
    continue
}
```

**Commit:** [`b6eb452`](https://github.com/HackCTF/kubevirt/commit/b6eb452)

### Fix 2: Pointer receiver en Alias

**Archivo:** `pkg/virt-launcher/virtwrap/api/schema.go:915-923`

`GetName()` y `IsUserDefined()` usan value receivers. Con nil `*Alias`, Go
copia el struct por valor pero el pointer es nil → SIGSEGV antes del body.

```go
// ANTES: value receiver — SIGSEGV en nil
func (alias Alias) GetName() string {
    return alias.name
}

// DESPUÉS: pointer receiver, nil-safe
func (alias *Alias) GetName() string {
    if alias == nil {
        return ""
    }
    return alias.name
}
```

**Commit:** [`d7b40d2`](https://github.com/HackCTF/kubevirt/commit/d7b40d2)

### Fix 3: Preservar UUID entre doble define

**Archivo:** `pkg/virt-launcher/virtwrap/nichotplug.go:199`

Libvirt genera UUID en el primer define. El segundo define tiene UUID diferente
→ libvirt rechaza: "already exists with different UUID".

```go
// DESPUÉS: UUID preservado
domainSpecWithoutIfacePlaceholders.Devices.DeepCopyInto(&domainSpec.Devices)
domainSpec.UUID = domainSpecWithoutIfacePlaceholders.UUID
```

**Commit:** [`293f0d7`](https://github.com/HackCTF/kubevirt/commit/293f0d7)

---

## Fix 4: Single-pass PCI topology

**Archivo:** `pkg/virt-launcher/virtwrap/nichotplug.go`

Reemplaza el two-pass (placeholder interfaces → define → leer XML →
remover interfaces → re-define) con una versión single-pass que agrega
directamente `pcie-root-port` controllers al domain spec.

**Problema original:** ~500-1000ms de latencia extra en creación de VMI.
Con Fix 4, `withNetworkIfacesResources` agrega `ReservedInterfaces - len(interfaces)`
slots PCI directamente.

```go
// DESPUÉS: single-pass
func withNetworkIfacesResources(...) (cli.VirDomain, error) {
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
            Model: "pcie-root-port",
        })
    }
    return f(vmi, domainSpec)
}
```

**Commit:** [`9000317`](https://github.com/HackCTF/kubevirt/commit/9000317)

**Dependency eliminada:** Fix 3 ya no es necesario — solo hay un
`virDomainDefineXML` call.

---

## Fix 4a: Controller index attribute

**Archivo:** `pkg/virt-launcher/virtwrap/nichotplug.go:172`

Libvirt requiere un `index` único para cada controller PCI.
Fix 4 original lo omitía → error XML.

```go
// DESPUÉS: con Index
domainSpec.Devices.Controllers = append(domainSpec.Devices.Controllers, api.Controller{
    Type:  "pci",
    Index: fmt.Sprintf("%d", i+1),   // ← Fix 4a
    Model: "pcie-root-port",
})
```

**Commit:** [`d304acc`](https://github.com/HackCTF/kubevirt/commit/d304acc)

---

## Netpod readiness watcher

**Archivos:**
- `pkg/network/setup/netpod/readiness.go` (258 líneas, nuevo)
- `pkg/network/setup/netpod/netpod.go` (+47 líneas)

Espera a que las interfaces de red secundarias (multus) estén listas via
netlink `RTM_NEWLINK` antes de que `Setup()` continúe.

**Opt-in via NetConf:** cuando `timeout=0`, el waiter se salta.

**Commits:** [`8c5c7d0`](https://github.com/HackCTF/kubevirt/commit/8c5c7d0),
[`8b8555b`](https://github.com/HackCTF/kubevirt/commit/8b8555b),
[`c36219f`](https://github.com/HackCTF/kubevirt/commit/c36219f)

---

## Guest-console-log sidecar fix

**Archivos:**
- `cmd/virt-launcher/virt-launcher.go` — el log de consola se crea siempre
  (no solo cuando `runWithNonRoot=false`)
- `cmd/virt-tail/main.go` — errores de timeout son non-fatal:
  `DirectoryTimeoutError`, `SocketTimeoutError`, `LogFileNotReadyError`

**Commit:** [`3e6309c`](https://github.com/HackCTF/kubevirt/commit/3e6309c)

---

## Pre-start hook TCG + multus

**Archivos:**
- `cmd/virt-launcher/virt-launcher.go` — función `runPreStartHook()`
- `cmd/virt-tail/main.go` — timeout configurable `VIRT_TAIL_SOCKET_TIMEOUT`
- `Dockerfile.hackctf.fix-tcg-multus`

### Problema

TCG (sin KVM) tiene QEMU boot lento (30-60s). Multus secondary NAD puede
no estar listo cuando virt-handler envía el comando start. La interface
secundaria no aparece → la VM no tiene connectividad.

### Solución

Pre-start hook dentro de virt-launcher que espera a que las interfaces
secundarias aparezcan en `/sys/class/net` antes de continuar.

```go
func runPreStartHook(vmiUID string, domainSpec interface{}) error {
    // 1. Asegurar que el directorio de logs existe
    // 2. Asegurar que el archivo de serial log existe
    // 3. Esperar interfaces secundarias (net1, net2) en /sys/class/net
    //    - Timeout configurable: VIRT_LAUNCHER_PRESTART_TIMEOUT (default 120s)
    //    - Best-effort: errores se loguean pero no previenen QEMU start
}
```

**Orden crítico:** El hook se ejecuta **DESPUÉS** de
`startDomainEventMonitoring()` para que la conexión libvirt no expire
durante la espera de 120s.

**Commit:** [`f38d937`](https://github.com/HackCTF/kubevirt/commit/f38d937)

---

## Dockerfile

**Archivo:** `Dockerfile.hackctf.fix-tcg-multus`

```dockerfile
FROM harbor.k8s.local/library/virt-launcher:fix-guest-console-log-v2

# Reemplazar virt-launcher binary con la versión del pre-start hook
COPY _out/virt-launcher /usr/bin/virt-launcher

# Reemplazar virt-tail binary con timeout configurable + non-fatal errors
COPY _out/virt-tail /usr/bin/virt-tail

# Verificar binarios
RUN /usr/bin/virt-launcher --help 2>&1 | head -3 || true
RUN /usr/bin/virt-tail --help 2>&1 | head -3 || /usr/bin/virt-tail --logfile=/tmp/test 2>&1 | head-3 || true

CMD ["virt-launcher"]
```

**Imagen resultante:** `harbor.k8s.local/library/virt-launcher:fix-tcg-multus-v1` (735MB)

**Contenido de la imagen (3 containers en el pod VMI):**

| Container | Binary | Propósito |
|-----------|--------|-----------|
| `compute` | `virt-launcher` | QEMU/libvirt + pre-start hook |
| `guest-console-log` | `virt-tail` | Stream del serial log |
| `container-disk-binary` (init) | `virt-launcher` | Init container para binarios |

Los 3 usan la misma imagen `fix-tcg-multus-v1`.

---

## Proceso de build

### Prerrequisitos

- **WSL2** con Go 1.22+ y libvirt-dev
- **Docker Desktop** en Windows (para build de imágenes)
- **Acceso a harbor.k8s.local** (desde Windows, no desde WSL2)
- **CRI-O** en los workers del cluster (no containerd)

### Workflow: WSL2 build + Windows push

WSL2 no puede resolver `harbor.k8s.local`, así que el workflow es:
1. Compilar en WSL2 (tiene libvirt-dev)
2. Copiar binarios a Windows
3. Build Docker en Windows (Docker Desktop)
4. Push a Harbor desde Windows

### Paso 1: Compilar binarios en WSL2

```bash
# En WSL2, dentro del repo kubevirt
cd /path/to/kubevirt

# Build virt-launcher (requiere CGO_ENABLED=1 + libvirt-dev)
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build \
    -o _out/virt-launcher \
    ./cmd/virt-launcher/

# Build virt-tail
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -o _out/virt-tail \
    ./cmd/virt-tail/

# Verificar
ls -la _out/virt-launcher _out/virt-tail
```

### Paso 2: Copiar binarios a Windows

```powershell
# En PowerShell desde Windows
wsl cp /path/to/kubevirt/_out/virt-launcher "D:\HackCTF\kubevirt\_out\"
wsl cp /path/to/kubevirt/_out/virt-tail "D:\HackCTF\kubevirt\_out\"
```

### Paso 3: Build Docker image

```powershell
# En PowerShell desde Windows
cd D:\HackCTF\kubevirt

docker build -t harbor.k8s.local/library/virt-launcher:fix-tcg-multus-v1 `
    -f Dockerfile.hackctf.fix-tcg-multus .
```

### Paso 4: Push a Harbor

```powershell
docker push harbor.k8s.local/library/virt-launcher:fix-tcg-multus-v1
```

### Paso 5: Limpiar cache CRI-O en workers

CRI-O cachea imágenes por tag. Si el tag no cambió, no re-pulla.

```bash
# En cada worker (SSH)
# 1. Escalar virt-handler DaemonSet a 0 para liberar la imagen
kubectl scale daemonset virt-handler -n kubevirt --replicas=0

# 2. Esperar que los pods terminen
kubectl wait --for=delete pod -l kubevirt.io=virt-handler -n kubevirt --timeout=120s

# 3. Eliminar imagen vieja de CRI-O
crictl rmi harbor.k8s.local/library/virt-launcher:fix-tcg-multus-v1 || true

# 4. Restaurar virt-handler
kubectl scale daemonset virt-handler -n kubevirt --replicas=1
```

---

## Despliegue en cluster Kubernetes

### Cluster Combate

- **Master:** 192.168.56.140 (thirsty-kapitsa)
- **Workers:** 192.168.56.141 (funny-einstein), 192.168.56.142 (nervous-vaughan)
- **Registry:** harbor.k8s.local (192.168.56.136)

### Paso 1: Verificar imagen en Harbor

```bash
curl -k -u admin:HarborAdmin2025!SecurePassword \
    https://harbor.k8s.local/api/v2.0/projects/library/repositories/virt-launcher/artifacts?page_size=5
```

### Paso 2: Patch virt-controller Deployment

El flag `--launcher-image` es LA fuente autoritativa para la imagen de
virt-launcher. El DaemonSet template NO tiene efecto en VMIs nuevas.

```bash
kubectl patch deployment virt-controller -n kubevirt --type json \
    -p='[{"op":"replace","path":"/spec/template/spec/containers/0/args/1","value":"harbor.k8s.local/library/virt-launcher:fix-tcg-multus-v1"}]'

kubectl rollout status deployment virt-controller -n kubevirt --timeout=120s
```

### Paso 3: Restaurar virt-handler DaemonSet

El DaemonSet debe tener el spec original de kubevirt (12 volumes,
init container `node-labeller.sh`, no `virt-launcher`).

```bash
kubectl replace -f /tmp/virt-handler-ds.yaml
```

El YAML del DaemonSet debe incluir:
- **Init container:** `/bin/sh -c node-labeller.sh` (NO `/usr/bin/virt-launcher`)
- **Volumes:** 12 volumes incluyendo `node-labeller` (hostPath), virtiofs, etc.
- **SecurityContext:** `privileged: true`
- **ImagePullPolicy:** `Always` (para forzar pull de nueva imagen)

### Paso 4: Limpiar pods viejos

```bash
# Eliminar pods virt-handler viejos para forzar recreación
kubectl delete pod -n kubevirt -l kubevirt.io=virt-handler

# Esperar que los nuevos arranquen
kubectl wait --for=condition=ready pod -l kubevirt.io=virt-handler -n kubevirt --timeout=300s
```

### Paso 5: Verificar componentes

```bash
# virt-handler: 2/2 Running
kubectl get daemonset virt-handler -n kubevirt

# virt-controller: 2/2 Running con nuevo --launcher-image
kubectl get deployment virt-controller -n kubevirt
kubectl get deploy virt-controller -n kubevirt -o jsonpath='{.spec.template.spec.containers[0].args}'

# virt-api: 2/2 Running
kubectl get deployment virt-api -n kubevirt
```

### Paso 6: Test VMI

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstance
metadata:
  name: test-new-image
  namespace: user-superadmin
spec:
  domain:
    cpu:
      cores: 1
    memory:
      guest: 256Mi
    machine:
      type: q35
    devices:
      disks:
      - name: containerdisk
        disk:
          bus: virtio
  volumes:
  - name: containerdisk
    containerDisk:
      image: harbor.k8s.local/library/cirros-container-disk-demo:latest
```

```bash
kubectl apply -f test-vmi.yaml
kubectl get vmi test-new-image -n user-superadmin -w
```

---

## Verificación

### Components Running

```
virt-api         2/2   Running
virt-controller  2/2   Running (--launcher-image=fix-tcg-multus-v1)
virt-handler     2/2   Running (DaemonSet original kubevirt spec)
```

### VMI Running with TCG

```
NAME             AGE   PHASE     IP            NODENAME
test-new-image   58s   Running   10.200.x.x   funny-einstein
```

### Pre-start hook execution

```
prestart: running hook for VMI <uid> (timeout=2m0s)
prestart: waiting for secondary interfaces: [net1]
prestart: timeout waiting for [net1] after 2m0s    ← esperado, VMI test no tiene secondary net
pre-start hook failed: timeout waiting for interfaces [net1] (continuing anyway)
Domain started.
```

### No libvirt panic

El hook se ejecuta DESPUÉS de `startDomainEventMonitoring()`, así que
la conexión libvirt permanece activa durante la espera.

### QEMU with TCG

```
qemu-kvm ... -accel tcg ...
```

---

## Troubleshooting

### "executable file not found in $PATH"

**Causa:** virt-handler DaemonSet tiene init container incorrecto.
El emptyDir `virt-bin-share-dir` en `/usr/bin` sobreescribe los
binarios de la imagen.

**Fix:** Restaurar DaemonSet con init container `node-labeller.sh`:

```bash
kubectl replace -f /tmp/virt-handler-ds.yaml
```

### "Connection to libvirt lost" + panic

**Causa:** Pre-start hook se ejecuta ANTES de `startDomainEventMonitoring()`.
El hook bloquea 120s, libvirt timeout → panic.

**Fix:** Asegurar que `startDomainEventMonitoring()` se llama ANTES del hook.
Orden correcto en `main()`:

```go
startDomainEventMonitoring(...)   // PRIMERO
runPreStartHook(...)              // DESPUÉS
```

### VMI stuck en Pending

**Causa:** virt-controller usa imagen vieja (`fix-guest-console-log-v2`).

**Fix:** Verificar flag `--launcher-image`:

```bash
kubectl get deploy virt-controller -n kubevirt -o jsonpath='{.spec.template.spec.containers[0].args}'
```

Debe mostrar `fix-tcg-multus-v1`. Si no, re-aplicar el patch.

### OOMKill en pods virt-launcher

**Causa:** QEMU bajo TCG necesita ~30% overhead de RAM sobre guest.

**Fix:** Aumentar memory limit:

| Guest RAM | Container limit mínimo |
|-----------|----------------------|
| 4Gi       | 6Gi                  |
| 6Gi       | 8Gi                  |
| 8Gi       | 11Gi                 |

### Pre-start hook timeout siempre

**Causa:** VMI no tiene interfaces secundarias (solo eth0).

**Comportamiento esperado:** El hook hace best-effort — si no hay
interfaces secundarias, loguea warning y continúa. QEMU arranca normal.

Para deshabilitar el hook completamente, setear env var:

```yaml
env:
- name: VIRT_LAUNCHER_PRESTART_TIMEOUT
  value: "0s"
```

### CRI-O no re-pulla imagen

**Causa:** CRI-O cachea por tag. Si el tag no cambió, usa la imagen cacheada.

**Fix:** Usar tag nuevo (`fix-tcg-multus-v1` en vez de `fix-tcg-multus-v0`)
o limpiar cache:

```bash
crictl rmi harbor.k8s.local/library/virt-launcher:fix-tcg-multus-v1
```

---

## Environment Variables

| Variable | Default | Descripción |
|----------|---------|-------------|
| `VIRT_TAIL_SOCKET_TIMEOUT` | `120s` | Timeout para socket de serial console en virt-tail |
| `VIRT_LAUNCHER_PRESTART_TIMEOUT` | `120s` | Timeout del pre-start hook para interfaces secundarias |
| `VIRT_LAUNCHER_SECONDARY_INTERFACES` | `net1` | Interfaces esperadas (comma-separated) |
| `VIRT_LAUNCHER_LOG_VERBOSITY` | `2` | Verbosity level de logs |

---

## Archivos modificados (resumen)

```
Dockerfile.hackctf.fix-tcg-multus          ← NUEVO (build de imagen custom)
cmd/virt-launcher/virt-launcher.go         ← pre-start hook + console log fix
cmd/virt-tail/main.go                      ← timeout configurable + non-fatal errors
pkg/virt-launcher/virtwrap/manager.go      ← nil guard (Fix P0-1)
pkg/virt-launcher/virtwrap/api/schema.go   ← pointer receiver (Fix P0-2)
pkg/virt-launcher/virtwrap/nichotplug.go   ← UUID fix + single-pass PCI (Fix P0-3, 4, 4a)
pkg/network/setup/netpod/netpod.go         ← readiness watcher integration
pkg/network/setup/netpod/readiness.go      ← NUEVO (netlink RTM_NEWLINK waiter)
```

---

## Tags de imagen

| Tag | Fixes | Estado |
|-----|-------|--------|
| `virt-launcher:fixed` | P0 (1+2+3) | Reemplazado |
| `virt-launcher:fixed-v4` | P0 + Fix 4 (sin Index) | Reemplazado |
| `virt-launcher:fixed-v5` | P0 + Fix 4 + Fix 4a | Reemplazado |
| `virt-launcher:fix-guest-console-log-v2` | guest-console-log fix | Base para siguiente |
| `virt-launcher:fix-tcg-multus-v1` | **P0 + Fix 4 + 4a + pre-start hook** | **ACTUAL** |

---

*Documento generado el 2026-07-14. Branch `hackctf/win10-tcg-fixes`.*
