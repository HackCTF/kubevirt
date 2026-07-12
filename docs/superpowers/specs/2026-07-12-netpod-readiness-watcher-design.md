# Race Condition Fix — Wait for Secondary Network Interfaces in NetPod.Setup()

> **For agentic workers:** This is a design spec. After approval, use `superpowers:writing-plans` to create an implementation plan with bite-sized tasks.

**Date:** 2026-07-12
**Author:** HackCTF infrastructure team
**Target branch:** `hackctf/win10-tcg-fixes`
**Affected code path:** `pkg/network/setup/netpod/netpod.go::Setup()` → `discover()` → "pod link (X) is missing"

---

## Problem Statement

KubeVirt's `virt-handler` triggers `setupNetwork()` when a VMI is scheduled and the launcher pod becomes responsive. At that moment, `NetPod.Setup()` calls `nmstateAdapter.Read()` to inspect the pod's network namespace. If Multus has not yet finished setting up the secondary networks (`net1`, `net2`, ...), `discover()` returns `pod link (X) is missing` and `setupNetwork()` fails.

The launcher pod is stuck in `ContainerCreating` indefinitely (or until virt-handler retries via the workqueue's exponential backoff, which starts at 5ms and grows). Manual workaround: `kubectl delete pod + kubectl apply` works because the second time Multus is warm.

This bug is documented in `D:\HackCTF\Vms\LAB_l2-attacks-01_VERIFICACION.md:771` as §7.3.

---

## Root Cause (verified)

The race window is between **CRI-O sandbox creation** and **Multus finishing CNI ADD for secondary networks**. virt-handler fires on VMI scheduling events, not on pod-network readiness events. The `nmstateAdapter.Read()` call is a one-shot snapshot — if any expected interface is missing, the function fails immediately.

`D:\HackCTF\kubevirt\pkg\network\setup\netpod\discover.go:55`:
```go
if !podIfaceExists {
    return fmt.Errorf("pod link (%s) is missing", podIfaceName)
}
```

`D:\HackCTF\kubevirt\pkg\virt-handler\vm.go:3065`:
```go
if err := d.setupNetwork(vmi, nonAbsentNets); err != nil {
    return fmt.Errorf("failed to configure vmi network: %w", err)
}
```

The workqueue retries via `AddRateLimited` (exponential backoff 5ms→1000s), but with disk pressure or heavy scheduling load the gap can grow faster than the backoff.

---

## Approach

**Option C: Netlink event watcher.** Subscribe to `RTM_NEWLINK` events in the pod's netns before calling `discover()`. Wait until all expected interfaces appear (or a configurable timeout elapses). This is event-driven, no polling, minimal CPU overhead.

### Why Option C

- **Event-driven**: wakes up immediately when CNI completes, no wasted polls
- **Efficient**: zero CPU between events, ideal for tight scheduling windows
- **Netlink support vendored**: `github.com/vishvananda/netlink/nl` provides `Subscribe()` and `SubscribeAt()` for netns-scoped subscriptions
- **Predictable behavior**: the watcher either gets the events or times out; no exponential backoff races with CNI

### Trade-offs accepted

- More complex than polling (goroutine, channel, cleanup)
- Need careful context cancellation to avoid leaks
- Tests require either real netlink (slow) or a netlink event source interface

---

## Architecture

### Components

1. **Interface readiness watcher** (new file `pkg/network/setup/netpod/readiness.go`)
   - Subscribes to netlink link events in a given netns
   - Returns a channel that yields when watched interfaces appear, or errors out on timeout

2. **Integration into `Setup()`** (modify `pkg/network/setup/netpod/netpod.go`)
   - Before `discover()` is called, compute the set of expected pod interface names (from `vmiSpecIfaces` and the network name scheme)
   - Run `waitForInterfaces()` inside the existing `NSExec.Do()` block (already inside the pod netns)
   - Only wait if `discover()` would fail on missing interfaces — if there are no pending networks, skip the wait

3. **Configuration** (add to `NetPod` struct)
   - `readinessTimeout time.Duration` (default 30s)
   - `readinessCheckInterval time.Duration` (default 100ms, for fallback polling)
   - Use the `WithXxx()` option pattern already used by `WithNMStateAdapter`, etc.

### Data flow

```
virt-handler
  └→ NetConf.Setup(vmi, networks, launcherPid, ...)
       └→ NetPod.Setup()
            └→ NSExec.Do(func() error {
                 currentStatus, _ := nmstateAdapter.Read()       // initial snapshot
                 
                 expectedIfaces := computeExpectedPodIfaces()      // NEW: set of names
                 err := waitForInterfaces(expectedIfaces)          // NEW: netlink subscribe + wait
                 if err != nil {
                     return err  // timeout or netlink error
                 }
                 
                 discover(currentStatus)                           // existing
                 config(currentStatus)                             // existing
               })
```

### Waiter implementation sketch

```go
// readiness.go
type netlinkWatcher interface {
    Subscribe(newNs netns.NsHandle) (subscriber, error)
}

type subscriber interface {
    Receive() ([]nl.NetlinkMessage, error)
    Close()
}

type readinessWaiter struct {
    timeout time.Duration
    watcher netlinkWatcher
}

func (w readinessWaiter) waitForInterfaces(currentNS netns.NsHandle, expected map[string]bool) error {
    sub, err := w.watcher.Subscribe(currentNS)
    if err != nil {
        return fmt.Errorf("failed to subscribe to netlink link events: %w", err)
    }
    defer sub.Close()
    
    deadline := time.Now().Add(w.timeout)
    seen := make(map[string]bool)
    
    for time.Now().Before(deadline) {
        remaining := time.Until(deadline)
        if remaining <= 0 {
            break
        }
        
        // Set recv timeout
        // ...
        
        msgs, err := sub.Receive()
        if err != nil {
            if isTimeout(err) {
                continue
            }
            return err
        }
        
        for _, msg := range msgs {
            if msg.Header.Type != unix.RTM_NEWLINK {
                continue
            }
            link := parseLinkMessage(msg)
            if expected[link.Name] {
                seen[link.Name] = true
                if allExpectedSeen(expected, seen) {
                    return nil
                }
            }
        }
    }
    
    return fmt.Errorf("timed out waiting for pod interfaces: missing %v", missingExpected(expected, seen))
}
```

### Important: We're already in the netns

The existing `n.state.NSExec.Do(func() error { ... })` block already enters the pod's netns. The netlink subscription should be created inside that block to listen on the right netns. The high-level helper `nl.SubscribeAt(newNs, curNs, protocol, groups...)` takes the netns handles explicitly.

---

## Error Handling

- **Netlink subscribe fails**: log warning, fall through to existing `discover()`. Netlink subscribe can fail in restricted containers; we shouldn't make things worse. The existing retry path (workqueue) will catch it.
- **Timeout**: return error, which propagates up to `setupNetwork()` → virt-handler → workqueue retry. Existing retry behavior remains.
- **Receive errors mid-wait**: log and return error.

Default timeout: 30 seconds. This is generous enough for slow CNI plugins but bounded to prevent stuck pods.

---

## Testing Strategy

### Unit tests (`pkg/network/setup/netpod/readiness_test.go`)

1. **Happy path**: feed a sequence of `RTM_NEWLINK` messages, verify `waitForInterfaces()` returns nil when all expected names appear
2. **Timeout**: don't feed any messages, verify timeout error
3. **Partial match**: feed one expected + one unrelated, verify still waits for the other
4. **Subscribe error**: mock the watcher to return error, verify fallback to `discover()` behavior (don't block)
5. **Cleanup**: verify the subscriber is closed even on error paths

### Integration tests

- Use the existing `nmstateStub` to simulate the pod's network status
- Mock the netlink watcher interface (not the real netlink) to avoid kernel dependencies in CI
- Add new `netpod_test.go` cases that exercise the wait path

### Manual cluster verification

1. Build `virt-launcher` image with the fix
2. Push to Harbor
3. Update `virt-controller` image in cluster
4. Recreate `virt-launcher-vm-fin-v2` repeatedly — verify it consistently gets `net1`
5. Repeat for all 4 VMI victims in the l2-attacks-v2 lab

---

## Files Affected

| File | Action | Purpose |
|------|--------|---------|
| `pkg/network/setup/netpod/readiness.go` | Create | Netlink-based interface readiness waiter |
| `pkg/network/setup/netpod/readiness_test.go` | Create | Unit tests for the waiter |
| `pkg/network/setup/netpod/netpod.go` | Modify | Integrate waiter into `Setup()` |
| `pkg/network/setup/netpod/netpod_test.go` | Modify | Add tests for the new behavior |
| `pkg/network/setup/netpod/BUILD.bazel` | Modify | Add new files to build |

No changes to `virt-handler/vm.go` — the fix is contained inside the netpod package.

---

## Configuration

| Field | Default | Purpose |
|-------|---------|---------|
| `readinessTimeout` | `30s` | Max time to wait for interfaces |
| `readinessCheckInterval` | `100ms` | Fallback poll interval (only used if netlink unreadable) |

Both fields are exposed via `WithReadinessTimeout()` and `WithReadinessCheckInterval()` options. The default 30s matches the kubelet's default pod startup timeout, so we won't hold up pods longer than kubelet would have anyway.

---

## Risks

1. **Netlink not available in restricted contexts**: fall back to existing behavior (no waiting) if subscribe fails. This is a safe degradation — the worst case is the original bug.
2. **Goroutine leak if cleanup fails**: defer `sub.Close()` everywhere, ensure it's called on all exit paths.
3. **Context cancellation**: the existing `Setup()` doesn't take a context. We rely on the timeout for cancellation. If we want better cancellation later, we can pass a context — out of scope for this fix.
4. **Multiple interfaces arriving simultaneously**: netlink messages are coalesced, but we process them all. Worst case is we wait one extra Receive() cycle, which is microseconds.

---

## Out of Scope

- Changes to `virt-handler/vm.go` retry logic (exponential backoff)
- Changes to `NetConf.Setup()` (caller)
- Changes to `nmstateAdapter` interface
- Migration to context-based cancellation
- Per-interface timeout (all interfaces share the same timeout for simplicity)

---

## Success Criteria

1. All existing `pkg/network/setup/netpod` tests still pass
2. New unit tests for the readiness waiter pass
3. Manual cluster test: recreate `virt-launcher-vm-fin-v2` 10 times in a row → all 10 must get `net1` without manual `kubectl delete + apply` workaround
4. No regression in `setupNetwork()` happy path performance
5. Image pushed to Harbor: `harbor.k8s.local/library/virt-launcher:hackctf-fix-netlink-race`