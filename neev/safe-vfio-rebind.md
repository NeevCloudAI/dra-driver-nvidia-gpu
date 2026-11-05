# Safe VFIO rebind (fork patch)

This fork carries two commits on top of upstream `v0.5.1` that make the
`PassthroughSupport` rebind of a GPU from `nvidia` to `vfio-pci` refuse to start
when it could hang. The branch exists only until upstream ships the same
behaviour; see [Dropping the patch](#dropping-the-patch).

| Commit | Upstream |
| --- | --- |
| Scan /proc for GPU holders instead of running fuser | cherry-pick of [#1504](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/pull/1504) (open) |
| Refuse a VFIO rebind that could hang instead of starting it (drain, fbdev guard, wait fix, this doc) | proposed in [#1612](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues/1612) |

## Background

With `PassthroughSupport`, the plugin advertises every GPU twice: as a
container GPU (`gpu-N`, bound to `nvidia`) and as a passthrough GPU
(`gpu-vfio-N`, bound to `vfio-pci`). The scheduler never hands out both views of
one card at once. When a claim asks for the VFIO view of a card that is on
`nvidia`, `VfioPciManager.Configure` moves the card:

```
echo <bdf> > /sys/bus/pci/drivers/nvidia/unbind
echo <bdf> > /sys/bus/pci/drivers/vfio-pci/bind
```

`Unconfigure` moves it back when the claim is released.

## The failure this prevents

The `nvidia` driver only releases a card nobody is using. If any client still
holds it when `unbind` is written, the driver logs

```
NVRM: Attempting to remove device 0000:01:00.0 with non-zero usage count!
```

and waits for that client forever. The wait happens inside the kernel while
holding the device lock, so:

- the writer can't be cancelled or killed, not even with `kill -9`;
- sysfs already shows the card with no driver;
- every later access to that device's sysfs blocks behind the lock, including
  the plugin's own `sriov_numvfs` read while listing GPUs;
- the plugin pod can't terminate, and its replacement hangs at
  `Traverse GPU devices` and never gets Ready, so **no** GPU on the node can be
  prepared, not just the stuck one.

Only a reboot recovers the node. This is what a node looked like after a bare
`unbind` was written while the card was held (process list from the host, PIDs
and timestamps trimmed):

```
bash             state=R  cpu≈21h   fd1 -> /sys/bus/pci/drivers/nvidia/unbind   # the writer, spinning in nvidia's remove
bash             state=D  wchan=device_release_driver_internal               # later unbind attempts, waiting on the lock
gpu-kubelet-plu  state=D  wchan=sriov_numvfs_show                            # old plugin (zombie) and its replacement
cat              state=D  wchan=driver_override_show                         # anyone who reads the device's sysfs
```

Stack of a blocked writer:

```
device_release_driver_internal+0x53/0x270
device_driver_detach+0x14/0x20
unbind_store+0xac/0xc0
...
```

## Why upstream v0.5.1 can still start that unbind

`Configure` already disables persistence mode, waits for the card to be free
(`WaitForGPUFree`) and runs the driver change in a goroutine with a 15s timeout
and an in-flight marker. Three gaps remain.

1. **The free check can't see containerized clients.** It runs
   `chroot <host-root> fuser <driver-root>/dev/nvidia<N>`. A container gets its
   own device node for the same device (`c 195:N`) at a different inode, and
   `fuser` matches by inode. Measured with a pod holding the GPU through NVML:

   ```
   $ chroot /host-root fuser -v /run/nvidia/driver/dev/nvidia0 ; echo rc=$?
   rc=1                                  # "free"
   $ stat -c '%n %i %t:%T' /run/nvidia/driver/dev/nvidia0 /dev/nvidia0
   /run/nvidia/driver/dev/nvidia0 177 c3:0
   /dev/nvidia0 1115 c3:0                # same device, different inode
   ```

   The #1504 commit replaces `fuser` with a scan of every fd in the host's
   `/proc` (mounted at `/proc` in the plugin) for character device `195:<minor>`,
   which found the holder: `187443 nvidia-smi :: 1 ctl 3 gpu0`.

2. **A client can attach between the check and the unbind.** Monitoring agents
   (dcgm-exporter, nvidia-smi based health checks, feature discovery) open the
   GPU for a moment on their own schedule. A check that passes guarantees
   nothing about the moment the unbind runs.

3. **The wait outlives the kubelet's call.** `WaitForGPUFree` waits up to 60s
   and ignores its context, while the kubelet cancels `NodePrepareResources`
   after 45s. The plugin keeps holding its prepare lock, the error naming the
   holder is lost, and the kubelet's retry fails on the lock:

   ```
   Warning FailedPrepareDynamicResources ... NodePrepareResources: rpc error: code = DeadlineExceeded desc = context deadline exceeded
   Warning FailedPrepareDynamicResources ... error acquiring prep/unprep lock: timeout acquiring lock (/var/lib/kubelet/plugins/gpu.nvidia.com/pu.lock)
   ```

## Design

NVML can drain a GPU: `nvidia-smi drain -p <bdf> -m 1`. A draining GPU refuses
new clients ("No devices were found", exit 6) while clients already attached
keep running. That closes gap 2: drain first, then check, then unbind.

### Configure (nvidia → vfio-pci)

```
 Configure(claim for gpu-vfio-N)
   │
   ├─ card already on vfio-pci? ───────────────────────────► done
   │
   ├─ evacuateGPU                                            (only when the card is on nvidia)
   │    ├─ nvidia_drm fbdev=Y and the card backs a framebuffer?
   │    │      └─ yes ─► refuse: "load nvidia_drm with fbdev=0"     (kernel holder, nothing can release it)
   │    ├─ persistence mode off
   │    ├─ DRAIN   nvidia-smi drain -p <bdf> -m 1
   │    │      └─ unsupported ─► log, continue with the scan alone
   │    └─ wait for no holders (scan /proc for 195:<minor>, every 1s)
   │           ├─ free ─────────────────────────────────────────────┐
   │           ├─ ctx cancelled (kubelet gave up)  ─► UNDRAIN, refuse │
   │           └─ 30s, still held ─► UNDRAIN, refuse:                 │
   │                "has open fds by process(es): <pid>(<exe>)"       │
   │                                                                  ▼
   ├─ change driver in a goroutine, 15s timeout (unchanged upstream logic)
   │      ├─ ok ─────────────────────────────────────────────────► done (card stays drained; it is off nvidia)
   │      ├─ error ─► UNDRAIN, refuse
   │      └─ timeout ─► refuse, leave drained                (the change may still be stuck in the kernel;
   │                                                          any NVML call on the card would block behind it)
```

### Unconfigure (vfio-pci → nvidia)

```
 change driver back to nvidia ─► UNDRAIN ─► persistence mode on
```

The undrain in `Unconfigure` is defensive: the card was drained when it left
`nvidia`, and a drained card that comes back would refuse every new client.

### Failure modes

| Situation | Result |
| --- | --- |
| Card free | Moves to `vfio-pci` in about 2s (persistence off, drain, scan, unbind, bind) |
| Card held by any process, in any container | Refused within 30s with the holders named; card undrained and left on `nvidia`; the kubelet retries |
| Holder exits during the retries | A later retry succeeds |
| Kubelet cancels the call | Wait returns at once, card undrained, prepare lock released |
| Drain unsupported on the GPU or driver | Logged; behaves like upstream plus #1504 |
| `nvidia_drm fbdev=1` with a framebuffer on the card | Refused before anything is changed |
| Driver change still times out | Refused; card left drained; the in-flight marker blocks retries in this process (upstream behaviour) |
| Plugin crashes between drain and unbind | Card stays drained on `nvidia`; undrain by hand (see below) |

### Known limits

- Kernel-side holders other than the fbdev console have no process fd, so
  neither the scan nor drain sees them. If one exists, the unbind can still
  hang; the 15s timeout keeps the plugin serving other requests in the same
  process.
- After a hang, a **restarted** plugin still blocks when it lists GPUs, because
  the listing (go-nvlib) reads every card's sysfs. Skipping a wedged card on
  startup needs a change in go-nvlib and is not part of this patch. Node-level
  health checks should take such a node out of scheduling.

## Code map

| Where | What |
| --- | --- |
| `cmd/gpu-kubelet-plugin/vfio-device.go` `Configure` | calls `evacuateGPU` for a card on `nvidia`; undrains on a failed (not timed-out) driver change |
| `vfio-device.go` `evacuateGPU` | fbdev guard, persistence off, drain, wait for holders, undrain on refusal |
| `vfio-device.go` `fbdevHoldsGPU` | `/sys/module/nvidia_drm/parameters/fbdev` is `Y` and `/sys/bus/pci/devices/<bdf>/graphics/fb*` exists |
| `vfio-device.go` `WaitForGPUFree` / `waitForDeviceFree` | 30s cap, returns on `ctx.Done()` |
| `vfio-device.go` `findCharDeviceHolders` | #1504: fd scan of `/proc` matching `st_rdev` |
| `vfio-device.go` `Unconfigure` | undrain after the card is back on `nvidia` |
| `cmd/gpu-kubelet-plugin/nvlib.go` `setGPUDrainState` | `chroot <driver-root> nvidia-smi drain -p <bdf> -m 0/1` |
| `VfioPciManager.{disablePersistence,setDrain,waitForFree}` | indirections so the sequence is unit-tested without a GPU |

## Verification

Unit tests (Linux; the scan reads `/proc`):

```
docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-mod=vendor \
  -e CGO_CFLAGS=-Wno-deprecated-declarations golang:1.26 \
  go test ./cmd/gpu-kubelet-plugin/ -run 'WaitForGPUFree|Evacuate|Fbdev|FindCharDevice|Undrain' -v
```

```
--- PASS: TestFindCharDeviceHolders
--- PASS: TestWaitForGPUFree
--- PASS: TestWaitForGPUFreeStopsWhenCancelled
--- PASS: TestFbdevHoldsGPU
--- PASS: TestFbdevHoldsGPUUnreadableParameter
--- PASS: TestEvacuateGPU
--- PASS: TestUndrainFailureIsOnlyLogged
```

On a node (RTX PRO 6000 Blackwell, driver 595.91.07, Kubernetes v1.37.1).

**Drain while held.** A host-side `nvidia-smi -l` holds the card; drain succeeds,
the holder keeps running, a new client is refused:

```
$ nvidia-smi drain -p 0000:01:00.0 -m 1
Successfully set GPU 00000000:01:00.0 drain state to: draining.
$ nvidia-smi -i 0000:01:00.0 --query-gpu=name --format=csv,noheader ; echo rc=$?
No devices were found
rc=6
$ nvidia-smi drain -p 0000:01:00.0 -m 0
Successfully set GPU 00000000:01:00.0 drain state to: not draining.
```

**Busy card is refused, not hung.** Same holder, then a pod with a
`vfio.gpu.nvidia.com` claim. Plugin log:

```
disableGPUPersistenceMode command: /busybox/chroot /driver-root nvidia-smi -i 0000:01:00.0 -pm 0
setGPUDrainState command: /busybox/chroot /driver-root nvidia-smi drain -p 0000:01:00.0 -m 1
  Successfully set GPU 00000000:01:00.0 drain state to: draining.
setGPUDrainState command: /busybox/chroot /driver-root nvidia-smi drain -p 0000:01:00.0 -m 0
  Successfully set GPU 00000000:01:00.0 drain state to: not draining.
```

Pod event, within the kubelet's deadline:

```
error configuring vfio device "gpu-vfio-0": error waiting for GPU "0000:01:00.0" to be free:
timed out waiting for gpu to be free: gpu device "0000:01:00.0" has open fds by process(es): 439611(nvidia-smi)
```

Card afterwards: `driver=nvidia`, `not draining`.

**Holder exits, retry succeeds:**

```
setGPUDrainState ... drain -p 0000:01:00.0 -m 1   Successfully set ... draining.
Returning newly prepared devices for claim '<ns>/<claim>': [{[vfio-gpu-0] <node> gpu-vfio-0 ...
```

Card: `driver=vfio-pci`. After the pod is deleted: `driver=nvidia`,
`not draining`, `nvidia-smi -L` lists the card.

**Free card, timing.** Persistence off at `:28.8`, claim prepared at `:30.6`;
back on `nvidia` in under a second on release.

## Operating

Check a card:

```
# from the driver container on the node
nvidia-smi drain -p <bdf> -q
basename "$(readlink /sys/bus/pci/devices/<bdf>/driver)"
```

Who holds a card (what the plugin's scan sees):

```
MINOR=$(awk '/Device Minor/{print $3}' /proc/driver/nvidia/gpus/<bdf>/information)
for f in /proc/[0-9]*/fd/*; do
  [ "$(stat -L -c %t:%T "$f" 2>/dev/null)" = "c3:$(printf %x $MINOR)" ] && echo "$f $(cat /proc/$(echo $f | cut -d/ -f3)/comm)"
done
```

A card left draining after a plugin crash refuses every new client. Undrain it:

```
nvidia-smi drain -p <bdf> -m 0
```

Never write to `/sys/bus/pci/drivers/nvidia/unbind` by hand on these nodes. If
something already did and the kernel logged `non-zero usage count`, take the
node out of scheduling and reboot it; nothing short of that releases the card.

## Build

```
TAG=v0.5.1-neev.<n>
docker buildx build --platform linux/amd64 --provenance=false \
  --build-arg GOLANG_VERSION="$(./hack/golang-version.sh)" \
  --build-arg TOOLKIT_CONTAINER_IMAGE="$(./hack/toolkit-container-image.sh)" \
  --build-arg VERSION="$TAG" --build-arg GIT_COMMIT="$(git rev-parse HEAD)" \
  -t <registry>/dra-driver-nvidia-gpu:$TAG -f deployments/container/Dockerfile --push .
```

Deploy by setting the chart's image repository and tag; the same image serves
the init container and the plugin container.

## Dropping the patch

Rebase onto each upstream release we adopt and drop whatever upstream has
merged:

- the #1504 commit once #1504 is released;
- the other commit once #1612 is fixed in a release.

When both are gone, go back to the upstream image and delete this branch.
