// go-virtio/rng — driver core: feature negotiation + init sequence +
// entropy-read path for the modern virtio-entropy device (virtio-rng,
// Virtio 1.1 §5.4).
//
// virtio-rng is the simplest device class in the spec: a single
// virtqueue (the "requestq"), no device-specific feature bits, and no
// device-config region. The driver places a device-writable buffer on
// the queue; the device fills it with random bytes and reports, via the
// used ring, how many bytes it wrote.
package rng

import (
	"github.com/go-virtio/common"
)

// RequestQueueIdx is the index of the single virtio-rng virtqueue
// (Virtio 1.1 §5.4.2 — "requestq", virtqueue 0).
const RequestQueueIdx uint16 = 0

// RequestQueueSize is the desired ring size for the request queue. A
// small ring is plenty: the driver keeps at most one buffer outstanding
// per Read call. Clamped down to the device's advertised maximum (and
// rounded to a power of two) during setup.
const RequestQueueSize uint16 = 8

// DefaultPollIterations is the busy-poll budget a plain Read spends
// waiting for the device to return a filled buffer. The entropy
// round-trip is sub-millisecond on every backend measured; this is a
// generous upper bound for the busy-poll model the driver uses.
const DefaultPollIterations = 200000

// AcceptedFeatures is the feature mask the driver negotiates ON.
// virtio-rng defines no device-specific feature bits (Virtio 1.1
// §5.4.3), so the only bit we ever accept is the non-negotiable
// VIRTIO_F_VERSION_1 (modern transport).
const AcceptedFeatures uint64 = common.FeatureVersion1

// AcceptFeatures returns the negotiated feature mask: the intersection
// of what the device offers and what we accept. The caller writes this
// back via DriverFeature.
//
// We require VIRTIO_F_VERSION_1 — if the device doesn't offer it, the
// device is legacy-only and we return ErrNotModernDevice.
func AcceptFeatures(deviceFeatures uint64) (uint64, error) {
	if deviceFeatures&common.FeatureVersion1 == 0 {
		return 0, ErrNotModernDevice
	}
	return deviceFeatures & AcceptedFeatures, nil
}

// VirtioRng wraps one initialised virtio-rng device. The caller holds
// this for the lifetime of the entropy source; the underlying virtqueue
// pages live as long as the supplied PageAllocator's lifetime contract.
type VirtioRng struct {
	// Cfg is the modern-transport handle (BARs + offsets + the
	// BARMemoryAccessor used for every register access).
	Cfg *common.ModernConfig

	// NegotiatedFeatures records what the driver-feature handshake
	// settled on. Exposed for diagnostic prints.
	NegotiatedFeatures uint64

	// transport is the underlying Transport — held so the entropy path
	// can route DMA-buffer allocations through the PageAllocator side.
	transport common.Transport

	// rq is the single request virtqueue set up by OpenVirtioRng.
	rq *common.Virtqueue
}

// OpenVirtioRng drives the full bring-up of one virtio-rng device:
//
//  1. Verify the PCI device ID is 0x1044 (modern entropy).
//  2. InitModernConfig walks PCI caps + populates the BAR locators.
//  3. Reset → ACK → DRIVER status progression.
//  4. Read DeviceFeature, mask to VERSION_1, write DriverFeature.
//  5. Set FEATURES_OK, verify it stuck.
//  6. Allocate + publish the request queue (queue 0).
//  7. DRIVER_OK status.
//
// On success the device is in DRIVER_OK state with an empty, ready
// request queue. Unlike virtio-net there is no device-config region to
// read and no buffers to pre-post — the driver posts a buffer on demand
// in Read.
func OpenVirtioRng(t common.Transport) (*VirtioRng, error) {
	// Sanity-check this really is a modern virtio-rng device.
	did, err := t.ReadConfig16(common.PCICfgDeviceID)
	if err != nil {
		return nil, err
	}
	if did != common.PCIDeviceIDModernEntropy {
		return nil, ErrInitWrongDeviceID
	}

	cfg, err := common.InitModernConfig(t)
	if err != nil {
		return nil, err
	}

	// Step 1: full reset (write 0 to DeviceStatus).
	if err := cfg.SetDeviceStatus(0); err != nil {
		return nil, err
	}
	// Spec §3.1.1: after reset DeviceStatus reads back as 0. We don't
	// sleep — the read itself is a firmware-liveness check; its value is
	// discarded.
	if _, err := cfg.DeviceStatus(); err != nil {
		return nil, err
	}

	// Step 2: ACKNOWLEDGE.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge); err != nil {
		return nil, err
	}
	// Step 3: DRIVER.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver); err != nil {
		return nil, err
	}

	// Step 4: read DeviceFeature, mask to our accepted set, write
	// DriverFeature.
	deviceFeats, err := cfg.DeviceFeatures64()
	if err != nil {
		return nil, err
	}
	if deviceFeats&common.FeatureVersion1 == 0 {
		return nil, ErrNotModernDevice
	}
	negotiated := deviceFeats & AcceptedFeatures
	if err := cfg.SetDriverFeatures64(negotiated); err != nil {
		return nil, err
	}

	// Step 5: FEATURES_OK + verify the device accepted our subset.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK); err != nil {
		return nil, err
	}
	status, err := cfg.DeviceStatus()
	if err != nil {
		return nil, err
	}
	if status&common.StatusFeaturesOK == 0 {
		return nil, ErrFeaturesNotOK
	}

	// Step 6: request-queue setup.
	rq, err := setupQueue(cfg, t, RequestQueueIdx, RequestQueueSize)
	if err != nil {
		return nil, err
	}

	// Step 7: DRIVER_OK.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK | common.StatusDriverOK); err != nil {
		return nil, err
	}

	return &VirtioRng{
		Cfg:                cfg,
		NegotiatedFeatures: negotiated,
		transport:          t,
		rq:                 rq,
	}, nil
}

// setupQueue performs the per-queue init: select, read max-size, write
// our size (= min(desired, max), rounded down to a power of two),
// allocate the Virtqueue, publish its descriptor/avail/used physical
// addresses, enable.
func setupQueue(cfg *common.ModernConfig, t common.Transport, queueIdx uint16, desiredSize uint16) (*common.Virtqueue, error) {
	if err := cfg.SelectQueue(queueIdx); err != nil {
		return nil, err
	}
	maxSize, err := cfg.QueueSize()
	if err != nil {
		return nil, err
	}
	if maxSize == 0 {
		// Device doesn't have this queue; spec says the driver must not
		// use it. (maxSize >= 1 from here on, so the size computed below
		// is always a non-zero power of two — no further zero-check
		// needed.)
		return nil, ErrQueueNotAvailable
	}
	size := desiredSize
	if size > maxSize {
		size = maxSize
	}
	// Round size DOWN to a power of two; some QEMU versions report
	// non-power-of-two QueueSize on legacy queues.
	for size&(size-1) != 0 {
		size &= size - 1
	}
	if err := cfg.SetQueueSize(size); err != nil {
		return nil, err
	}
	notifyOff, err := cfg.QueueNotifyOff()
	if err != nil {
		return nil, err
	}
	q, err := common.NewVirtqueue(t, size, queueIdx, notifyOff)
	if err != nil {
		return nil, err
	}
	descAddr := q.BasePhys + uint64(q.Layout.DescTableOffset)
	availAddr := q.BasePhys + uint64(q.Layout.AvailRingOffset)
	usedAddr := q.BasePhys + uint64(q.Layout.UsedRingOffset)
	if err := cfg.SetQueueDesc(descAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDriver(availAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDevice(usedAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueEnable(1); err != nil {
		return nil, err
	}
	return q, nil
}

// RequestQueue exposes the request *common.Virtqueue handle. Read-only
// accessor so callers can inspect ring state for diagnostic dumps; the
// field itself stays unexported.
func (r *VirtioRng) RequestQueue() *common.Virtqueue { return r.rq }

// Read fills p with entropy from the device, blocking (busy-poll) up to
// DefaultPollIterations per device round-trip. It always fills the whole
// of p on success (io.ReadFull-style semantics, matching crypto/rand's
// Reader contract) and returns len(p). A partial result is returned
// alongside an error if the device stalls or returns no bytes.
func (r *VirtioRng) Read(p []byte) (int, error) {
	return r.ReadPoll(p, DefaultPollIterations)
}

// ReadPoll is the parameterised variant of Read that takes an explicit
// busy-poll budget (iterations spent waiting for each device
// round-trip). Useful for callers that want a tighter timeout than
// DefaultPollIterations.
func (r *VirtioRng) ReadPoll(p []byte, pollIterations int) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// One DMA buffer, reused across chunks: virtio-rng can return fewer
	// bytes than requested, and p may exceed one page, so we loop.
	phys, mem, err := r.transport.AllocatePages(1)
	if err != nil {
		return 0, err
	}
	if phys == 0 {
		return 0, common.ErrAllocReturnedZero
	}
	addr := uintptr(phys) // identity-mapped on supported hosts
	total := 0
	for total < len(p) {
		want := len(p) - total
		if want > len(mem) {
			want = len(mem)
		}
		n, err := r.fillOnce(addr, phys, uint32(want), pollIterations)
		if err != nil {
			return total, err
		}
		if n == 0 {
			// Device acknowledged the buffer but wrote nothing; treat as
			// a stall rather than spin forever.
			return total, ErrReadTimeout
		}
		copy(p[total:], readBufferBytes(addr, n))
		total += n
	}
	return total, nil
}

// fillOnce posts a single device-writable buffer of `want` bytes, rings
// the doorbell, and busy-polls the used ring for the device's reply. It
// returns the number of bytes the device reported writing (clamped to
// `want`), or ErrReadTimeout if the budget is exhausted.
func (r *VirtioRng) fillOnce(addr uintptr, phys uint64, want uint32, pollIterations int) (int, error) {
	descIdx, err := r.rq.AddBuffer(addr, phys, want, true)
	if err != nil {
		return 0, err
	}
	if err := r.Cfg.NotifyQueue(RequestQueueIdx, r.rq.NotifyOff); err != nil {
		return 0, err
	}
	for spin := 0; spin < pollIterations; spin++ {
		gotIdx, length, ok := r.rq.PollUsed()
		if !ok {
			continue
		}
		_ = r.rq.Reclaim(gotIdx)
		got := int(length)
		if got > int(want) {
			got = int(want)
		}
		return got, nil
	}
	// Timed out with the descriptor still outstanding; free it so a
	// later Read can reuse the slot.
	_ = r.rq.Reclaim(descIdx)
	return 0, ErrReadTimeout
}

// Sentinel errors for the virtio-rng path. All exported so callers can
// branch + format them.
var (
	ErrNotModernDevice   = commonRngError("go-virtio/rng: device doesn't offer VIRTIO_F_VERSION_1 (legacy-only)")
	ErrFeaturesNotOK     = commonRngError("go-virtio/rng: FEATURES_OK status bit didn't stick after DriverFeature write")
	ErrInitWrongDeviceID = commonRngError("go-virtio/rng: PCI device ID is not 0x1044 (modern entropy device)")
	ErrQueueNotAvailable = commonRngError("go-virtio/rng: device reports QueueSize=0 for the request queue")
	ErrReadTimeout       = commonRngError("go-virtio/rng: read poll timeout (device returned no entropy within budget)")
)

// commonRngError is the package's tiny sentinel-error type — same
// pattern as go-virtio/common.commonError and go-virtio/net.commonNetError.
type commonRngError string

// Error implements the `error` interface.
func (e commonRngError) Error() string { return string(e) }
