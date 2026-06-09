# go-virtio/rng

Pure-Go virtio-rng (virtio-entropy) driver targeting the
`go-virtio/common` transport interfaces. Implements the modern-transport
(Virtio 1.0+) init sequence and the single-virtqueue entropy-read path
for the standard PCI-bound virtio-rng device (VID 0x1AF4, DID 0x1044).

virtio-rng is the simplest device class in the spec (Virtio 1.1 §5.4):
one virtqueue (the "requestq"), no device-specific feature bits, and no
device-config region. The driver posts a device-writable buffer on the
queue; the device fills it with random bytes and reports — via the used
ring — how many bytes it wrote.

This package owns the spec-level driver (the feature-acceptance mask, the
init sequence per Virtio 1.1 §3.1.1, the request-queue state machine) and
routes every transport-level operation through `go-virtio/common`'s
`Transport` interface. Drop in any implementation of that interface
(UEFI's `EFI_PCI_IO_PROTOCOL`, bare-metal MMIO, virtio-mmio adapter) and
the same driver code drives the device.

## Quick start

```go
import (
    virtiorng "github.com/go-virtio/rng"
)

// transport is any value that implements go-virtio/common.Transport.
vr, err := virtiorng.OpenVirtioRng(transport)
if err != nil {
    return err
}

// Read always fills the whole buffer on success (io.ReadFull-style,
// matching crypto/rand's Reader contract).
buf := make([]byte, 32)
if _, err := vr.Read(buf); err != nil {
    return err
}

// ReadPoll takes an explicit busy-poll budget for tighter timeouts.
n, err := vr.ReadPoll(buf, 50000)
```

`OpenVirtioRng` leaves the device in DRIVER_OK state with an empty,
ready request queue. Unlike virtio-net there is no device-config region
to read and no buffers to pre-post — the driver posts a buffer on demand
in `Read`.

## Sibling packages

  - [`github.com/go-virtio/common`](https://github.com/go-virtio/common)
    — transport-agnostic infrastructure (PCI cap walker, modern config
    layout, split-virtqueue impl, transport interfaces).
  - [`github.com/go-virtio/net`](https://github.com/go-virtio/net) —
    pure-Go virtio-net driver (the reference per-device-class driver this
    package mirrors).
  - [`github.com/go-virtio/blk`](https://github.com/go-virtio/blk) —
    placeholder for a future pure-Go virtio-blk driver.

## Note on the device ID

The modern virtio-entropy PCI device ID (`0x1044`) lives in
`go-virtio/common` as `PCIDeviceIDModernEntropy`, alongside
`PCIDeviceIDModern{Net,Block}` and the `PCIDeviceIDIsEntropy` helper.
Because those constants were added after common's `v0.1.0` tag, this
module currently carries a `replace github.com/go-virtio/common =>
../common` bridge in `go.mod`; drop it once common is re-tagged (`v0.1.1`)
and bump the `require`.

## License

BSD-3-Clause. See [LICENSE](LICENSE).
