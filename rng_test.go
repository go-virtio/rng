// End-to-end tests for the OpenVirtioRng driver path + the entropy
// Read path. Uses a fakeRngDevice transport that:
//
//   - Publishes a valid virtio-rng PCI config-space cap chain
//     (CommonCfg + extended NotifyCfg, no DeviceCfg — virtio-rng has no
//     device-config region).
//   - Tracks COMMON_CFG register state: the device-status progression,
//     feature-select index, and the single request queue's address
//     publication.
//   - Simulates the device side of the entropy round-trip: on a doorbell
//     write to the request queue it reads the just-published descriptor,
//     fills its buffer with a byte pattern, and posts a used-ring entry
//     reporting how many bytes it wrote.
//
// injectTransport wraps the fake to force a transport-level error (or a
// zero physical address) on the Nth call to a chosen method, which lets
// the error-return branches of OpenVirtioRng / setupQueue / ReadPoll be
// exercised deterministically.

package rng

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	"github.com/go-virtio/common"
)

var le = binary.LittleEndian

// fakeRngDevice is a minimal in-memory virtio-rng device for driver tests.
type fakeRngDevice struct {
	mu sync.Mutex

	// PCI config-space contents.
	cfg []byte

	// COMMON_CFG state.
	deviceFeatureSelect uint32
	deviceFeatures      uint64 // what the device offers
	driverFeatures      uint64 // what the driver acked
	deviceStatus        uint8
	currentQueue        uint16
	// Per-queue state. Key: queue idx.
	qsize      map[uint16]uint16
	qenable    map[uint16]uint16
	qdesc      map[uint16]uint64
	qdriver    map[uint16]uint64
	qdevice    map[uint16]uint64
	qnotifyOff map[uint16]uint16

	// BAR memory store (other reads/writes).
	bar map[uint64]uint64 // key = (bar<<48 | offset)

	// FEATURES_OK gate override: when true the device always clears the
	// FEATURES_OK bit, simulating a device that rejects our feature set.
	clearFeaturesOK bool

	// Entropy-fill behaviour for the request queue.
	fills    bool // publish a used-ring entry on notify
	fillByte byte // pattern base written into the buffer
	fillLen  int  // bytes to write/report; -1 = the whole requested buffer
	overfill bool // report a length larger than the buffer (clamp test)

	// heldPages pins references to allocated pages so the GC does not
	// reclaim them — the driver retains addresses via uintptr which the
	// GC doesn't trace.
	heldPages [][]byte
	allocFail bool
}

func newFakeRngDevice(deviceFeats uint64) *fakeRngDevice {
	d := &fakeRngDevice{
		deviceFeatures: deviceFeats,
		qsize:          map[uint16]uint16{0: 32},
		qenable:        map[uint16]uint16{},
		qdesc:          map[uint16]uint64{},
		qdriver:        map[uint16]uint64{},
		qdevice:        map[uint16]uint64{},
		qnotifyOff:     map[uint16]uint16{0: 0},
		bar:            map[uint64]uint64{},
		fills:          true,
		fillByte:       0xA5,
		fillLen:        -1,
	}
	d.cfg = buildVirtioRngCfgSpace()
	return d
}

func barKey(bar uint8, off uint64) uint64 { return uint64(bar)<<48 | off }

// PCIConfigReader.
func (d *fakeRngDevice) ReadConfig8(off uint8) (uint8, error) {
	if int(off) >= len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return d.cfg[off], nil
}
func (d *fakeRngDevice) ReadConfig16(off uint8) (uint16, error) {
	if int(off)+2 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint16(d.cfg[off : off+2]), nil
}
func (d *fakeRngDevice) ReadConfig32(off uint8) (uint32, error) {
	if int(off)+4 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint32(d.cfg[off : off+4]), nil
}

// PageAllocator.
func (d *fakeRngDevice) AllocatePages(count int) (uint64, []byte, error) {
	if d.allocFail {
		return 0, nil, errors.New("alloc fail")
	}
	mem := make([]byte, count*int(common.PageSize))
	addr := uintptr(0)
	if len(mem) > 0 {
		d.heldPages = append(d.heldPages, mem)
		addr = uintptrFromSlice(mem)
	}
	return uint64(addr), mem, nil
}

func (d *fakeRngDevice) commonCfgBAR() uint8     { return 0 }
func (d *fakeRngDevice) commonCfgOffset() uint64 { return 0 }

// BARMemoryAccessor.
func (d *fakeRngDevice) Read8(bar uint8, off uint64) (uint8, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceStatus:
			return d.deviceStatus, nil
		case common.CfgConfigGeneration:
			return 0, nil
		}
	}
	return uint8(d.bar[barKey(bar, off)] & 0xFF), nil
}

func (d *fakeRngDevice) Read16(bar uint8, off uint64) (uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgNumQueues:
			return 1, nil
		case common.CfgQueueSelect:
			return d.currentQueue, nil
		case common.CfgQueueSize:
			return d.qsize[d.currentQueue], nil
		case common.CfgQueueEnable:
			return d.qenable[d.currentQueue], nil
		case common.CfgQueueNotifyOff:
			return d.qnotifyOff[d.currentQueue], nil
		}
	}
	return uint16(d.bar[barKey(bar, off)] & 0xFFFF), nil
}

func (d *fakeRngDevice) Read32(bar uint8, off uint64) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			return d.deviceFeatureSelect, nil
		case common.CfgDeviceFeature:
			if d.deviceFeatureSelect == 0 {
				return uint32(d.deviceFeatures & 0xFFFFFFFF), nil
			}
			return uint32(d.deviceFeatures >> 32), nil
		}
	}
	return uint32(d.bar[barKey(bar, off)] & 0xFFFFFFFF), nil
}

func (d *fakeRngDevice) Read64(bar uint8, off uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			return d.qdesc[d.currentQueue], nil
		case common.CfgQueueDriver:
			return d.qdriver[d.currentQueue], nil
		case common.CfgQueueDevice:
			return d.qdevice[d.currentQueue], nil
		}
	}
	return d.bar[barKey(bar, off)], nil
}

func (d *fakeRngDevice) Write8(bar uint8, off uint64, v uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() && off-d.commonCfgOffset() == common.CfgDeviceStatus {
		// Simulate the FEATURES_OK handshake. virtio-rng requires no
		// device-specific bits, so the only requirement is VERSION_1
		// (which the driver always acks) — unless the test forces a
		// rejection via clearFeaturesOK.
		if v&common.StatusFeaturesOK != 0 {
			if d.clearFeaturesOK || d.driverFeatures&common.FeatureVersion1 == 0 {
				v &^= common.StatusFeaturesOK
			}
		}
		d.deviceStatus = v
		return nil
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeRngDevice) Write16(bar uint8, off uint64, v uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueSelect:
			d.currentQueue = v
			return nil
		case common.CfgQueueSize:
			d.qsize[d.currentQueue] = v
			return nil
		case common.CfgQueueEnable:
			d.qenable[d.currentQueue] = v
			return nil
		}
	}
	if off >= 0x1000 && off < 0x2000 {
		d.handleNotify(v)
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeRngDevice) Write32(bar uint8, off uint64, v uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			d.deviceFeatureSelect = v
			return nil
		case common.CfgDriverFeatureSelect:
			d.bar[barKey(bar, off)] = uint64(v)
			return nil
		case common.CfgDriverFeature:
			sel := d.bar[barKey(bar, common.CfgDriverFeatureSelect)]
			if sel == 0 {
				d.driverFeatures = (d.driverFeatures &^ 0xFFFFFFFF) | uint64(v)
			} else {
				d.driverFeatures = (d.driverFeatures & 0xFFFFFFFF) | (uint64(v) << 32)
			}
			return nil
		}
	}
	// virtio-rng's notify_off_multiplier is 4, so the doorbell is a
	// uint32 MMIO write (common.NotifyQueue widens it).
	if off >= 0x1000 && off < 0x2000 {
		d.handleNotify(uint16(v))
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeRngDevice) Write64(bar uint8, off uint64, v uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			d.qdesc[d.currentQueue] = v
			return nil
		case common.CfgQueueDriver:
			d.qdriver[d.currentQueue] = v
			return nil
		case common.CfgQueueDevice:
			d.qdevice[d.currentQueue] = v
			return nil
		}
	}
	d.bar[barKey(bar, off)] = v
	return nil
}

// handleNotify simulates the device-side reaction to a request-queue
// doorbell: read the most-recently-published descriptor, fill its buffer
// with a byte pattern, and post a used-ring entry reporting the count.
func (d *fakeRngDevice) handleNotify(qIdx uint16) {
	if !d.fills || qIdx != RequestQueueIdx {
		return
	}
	descAddr := d.qdesc[qIdx]
	availAddr := d.qdriver[qIdx]
	usedAddr := d.qdevice[qIdx]
	if descAddr == 0 || availAddr == 0 || usedAddr == 0 {
		return
	}
	size := d.qsize[qIdx]
	availSlice := readBufferBytes(uintptr(availAddr), 4+2*int(size))
	if availSlice == nil {
		return
	}
	availIdx := le.Uint16(availSlice[2:4])
	if availIdx == 0 {
		return
	}
	lastSlot := (availIdx - 1) % size
	descIdx := le.Uint16(availSlice[4+lastSlot*2 : 4+lastSlot*2+2])

	descSlice := readBufferBytes(uintptr(descAddr), 16*int(size))
	if descSlice == nil {
		return
	}
	o := int(descIdx) * 16
	bufAddr := le.Uint64(descSlice[o : o+8])
	bufLen := le.Uint32(descSlice[o+8 : o+12])

	write := int(bufLen)
	if d.fillLen >= 0 && d.fillLen < write {
		write = d.fillLen
	}
	buf := readBufferBytes(uintptr(bufAddr), write)
	for i := range buf {
		buf[i] = d.fillByte + byte(i)
	}

	reported := uint32(write)
	if d.overfill {
		reported = bufLen + 10
	}

	usedSlice := readBufferBytes(uintptr(usedAddr), 4+8*int(size))
	if usedSlice == nil {
		return
	}
	usedIdx := le.Uint16(usedSlice[2:4])
	slot := usedIdx % size
	uo := 4 + int(slot)*8
	le.PutUint32(usedSlice[uo:uo+4], uint32(descIdx))
	le.PutUint32(usedSlice[uo+4:uo+8], reported)
	le.PutUint16(usedSlice[2:4], usedIdx+1)
}

// buildVirtioRngCfgSpace builds a 256-byte PCI config-space buffer with
// a virtio-rng cap chain:
//
//	0x00 VID=0x1AF4 DID=0x1044
//	0x06 Status[CapList]=1
//	0x34 CapPtr=0x40
//	0x40 CommonCfg cap (16 bytes) BAR=0 offset=0 length=0x38
//	0x50 NotifyCfg ext cap (20 bytes) BAR=0 offset=0x1000 length=0x100
//	     [+16..+20] = 4 (notify_off_multiplier); next = end
//
// No DeviceCfg cap — virtio-rng has no device-config region.
func buildVirtioRngCfgSpace() []byte {
	cfg := make([]byte, 256)
	le.PutUint16(cfg[0:], common.PCIVendorID)
	le.PutUint16(cfg[2:], common.PCIDeviceIDModernEntropy)
	le.PutUint16(cfg[6:], common.PCIStatusCapabilityList)
	cfg[0x34] = 0x40

	// CommonCfg cap at 0x40.
	cfg[0x40] = common.PCICapIDVendorSpecific
	cfg[0x41] = 0x50 // next
	cfg[0x42] = 16   // cap_len
	cfg[0x43] = common.PCICapCommonCfg
	cfg[0x44] = 0                  // bar
	cfg[0x45] = 0                  // id
	le.PutUint32(cfg[0x48:], 0)    // offset
	le.PutUint32(cfg[0x4C:], 0x38) // length

	// NotifyCfg ext cap at 0x50, 20 bytes, next = end.
	cfg[0x50] = common.PCICapIDVendorSpecific
	cfg[0x51] = 0x00 // next = end
	cfg[0x52] = 20   // cap_len (extended)
	cfg[0x53] = common.PCICapNotifyCfg
	cfg[0x54] = 0
	cfg[0x55] = 0
	le.PutUint32(cfg[0x58:], 0x1000) // offset
	le.PutUint32(cfg[0x5C:], 0x100)  // length
	le.PutUint32(cfg[0x60:], 4)      // notify_off_multiplier

	return cfg
}

// --- happy-path + semantic tests --------------------------------------

func TestOpenVirtioRng_Success(t *testing.T) {
	d := newFakeRngDevice(common.FeatureVersion1)
	v, err := OpenVirtioRng(d)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	if v.NegotiatedFeatures != common.FeatureVersion1 {
		t.Errorf("Negotiated: got 0x%x, want 0x%x", v.NegotiatedFeatures, common.FeatureVersion1)
	}
	if v.RequestQueue() == nil {
		t.Error("RequestQueue nil")
	}
}

func TestOpenVirtioRng_IgnoresExtraDeviceBits(t *testing.T) {
	// Device offers spurious high feature bits; the driver must mask
	// them all out and negotiate only VERSION_1.
	d := newFakeRngDevice(common.FeatureVersion1 | (1 << 40) | (1 << 5))
	v, err := OpenVirtioRng(d)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	if v.NegotiatedFeatures != common.FeatureVersion1 {
		t.Errorf("Negotiated: got 0x%x, want 0x%x", v.NegotiatedFeatures, common.FeatureVersion1)
	}
}

func TestAcceptFeatures(t *testing.T) {
	if got, err := AcceptFeatures(common.FeatureVersion1 | (1 << 7)); err != nil || got != common.FeatureVersion1 {
		t.Errorf("AcceptFeatures(modern): got 0x%x, %v", got, err)
	}
	if _, err := AcceptFeatures(1 << 7); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("AcceptFeatures(legacy): got %v, want ErrNotModernDevice", err)
	}
}

func TestOpenVirtioRng_WrongDeviceID(t *testing.T) {
	d := newFakeRngDevice(common.FeatureVersion1)
	le.PutUint16(d.cfg[2:], common.PCIDeviceIDModernNet) // pretend to be virtio-net
	if _, err := OpenVirtioRng(d); !errors.Is(err, ErrInitWrongDeviceID) {
		t.Errorf("got %v, want ErrInitWrongDeviceID", err)
	}
}

func TestOpenVirtioRng_LegacyDevice(t *testing.T) {
	d := newFakeRngDevice(1 << 7) // no VERSION_1
	if _, err := OpenVirtioRng(d); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("got %v, want ErrNotModernDevice", err)
	}
}

func TestOpenVirtioRng_FeaturesNotOK(t *testing.T) {
	d := newFakeRngDevice(common.FeatureVersion1)
	d.clearFeaturesOK = true
	if _, err := OpenVirtioRng(d); !errors.Is(err, ErrFeaturesNotOK) {
		t.Errorf("got %v, want ErrFeaturesNotOK", err)
	}
}

func TestOpenVirtioRng_QueueZeroSize(t *testing.T) {
	d := newFakeRngDevice(common.FeatureVersion1)
	d.qsize[0] = 0
	if _, err := OpenVirtioRng(d); !errors.Is(err, ErrQueueNotAvailable) {
		t.Errorf("got %v, want ErrQueueNotAvailable", err)
	}
}

func TestOpenVirtioRng_QueueSizeClampAndRound(t *testing.T) {
	// maxSize=6 is below the desired 8 (exercises the clamp) and is not a
	// power of two (exercises the round-down loop): 6 → 4.
	d := newFakeRngDevice(common.FeatureVersion1)
	d.qsize[0] = 6
	v, err := OpenVirtioRng(d)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	if got := v.RequestQueue().Layout.Size; got != 4 {
		t.Errorf("queue size: got %d, want 4 (clamped 8→6, rounded 6→4)", got)
	}
}

func TestSentinelError(t *testing.T) {
	// The sentinel type's Error() method must round-trip its message.
	if got := ErrReadTimeout.Error(); got != string(ErrReadTimeout) {
		t.Errorf("Error(): got %q", got)
	}
}

// --- entropy Read path ------------------------------------------------

func TestRead_RoundTrip(t *testing.T) {
	d := newFakeRngDevice(common.FeatureVersion1)
	v, err := OpenVirtioRng(d)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	p := make([]byte, 16)
	n, err := v.Read(p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != len(p) {
		t.Errorf("n: got %d, want %d", n, len(p))
	}
	// Device fills with fillByte + index.
	for i := range p {
		if want := d.fillByte + byte(i); p[i] != want {
			t.Errorf("p[%d]: got 0x%x, want 0x%x", i, p[i], want)
		}
	}
}

func TestRead_MultiPage(t *testing.T) {
	// Larger than one page: exercises the want>len(mem) clamp and the
	// multi-chunk loop.
	d := newFakeRngDevice(common.FeatureVersion1)
	v, err := OpenVirtioRng(d)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	p := make([]byte, int(common.PageSize)+100)
	n, err := v.Read(p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != len(p) {
		t.Errorf("n: got %d, want %d", n, len(p))
	}
	if p[0] != d.fillByte {
		t.Errorf("p[0]: got 0x%x, want 0x%x", p[0], d.fillByte)
	}
}

func TestRead_ZeroLen(t *testing.T) {
	d := newFakeRngDevice(common.FeatureVersion1)
	v, err := OpenVirtioRng(d)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	n, err := v.Read(nil)
	if err != nil || n != 0 {
		t.Errorf("Read(nil): got (%d, %v), want (0, nil)", n, err)
	}
}

func TestRead_Overfill(t *testing.T) {
	// Device reports more bytes than the buffer holds; the driver must
	// clamp to the requested length.
	d := newFakeRngDevice(common.FeatureVersion1)
	d.overfill = true
	v, err := OpenVirtioRng(d)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	p := make([]byte, 16)
	n, err := v.Read(p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != len(p) {
		t.Errorf("n: got %d, want %d (should clamp)", n, len(p))
	}
}

func TestReadPoll_Timeout(t *testing.T) {
	d := newFakeRngDevice(common.FeatureVersion1)
	v, err := OpenVirtioRng(d)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	d.fills = false // device never completes the request
	if _, err := v.ReadPoll(make([]byte, 8), 100); !errors.Is(err, ErrReadTimeout) {
		t.Errorf("got %v, want ErrReadTimeout", err)
	}
}

func TestRead_ZeroBytesReturned(t *testing.T) {
	// Device acks the buffer but writes zero bytes — must not spin.
	d := newFakeRngDevice(common.FeatureVersion1)
	d.fillLen = 0
	v, err := OpenVirtioRng(d)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	if _, err := v.Read(make([]byte, 8)); !errors.Is(err, ErrReadTimeout) {
		t.Errorf("got %v, want ErrReadTimeout", err)
	}
}

func TestRead_AllocFail(t *testing.T) {
	d := newFakeRngDevice(common.FeatureVersion1)
	v, err := OpenVirtioRng(d)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	d.allocFail = true
	if _, err := v.Read(make([]byte, 8)); err == nil {
		t.Error("expected alloc error")
	}
}

func TestRead_AllocZeroPhys(t *testing.T) {
	d := newFakeRngDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioRng(it)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	it.enable = true
	it.zeroPhys = true
	if _, err := v.Read(make([]byte, 8)); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v, want ErrAllocReturnedZero", err)
	}
}

func TestRead_NotifyFail(t *testing.T) {
	d := newFakeRngDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioRng(it)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	it.enable = true
	it.fp = failPoint{"Write32", 1} // the request-queue doorbell
	if _, err := v.Read(make([]byte, 8)); err == nil {
		t.Error("expected notify error")
	}
}

func TestRead_QueueFull(t *testing.T) {
	d := newFakeRngDevice(common.FeatureVersion1)
	v, err := OpenVirtioRng(d)
	if err != nil {
		t.Fatalf("OpenVirtioRng: %v", err)
	}
	// Saturate the request queue so fillOnce's AddBuffer fails.
	q := v.RequestQueue()
	phys, _, _ := d.AllocatePages(1)
	for i := uint16(0); i < q.Layout.Size; i++ {
		if _, err := q.AddBuffer(uintptr(phys), phys, 8, true); err != nil {
			t.Fatalf("pre-fill AddBuffer[%d]: %v", i, err)
		}
	}
	if _, err := v.Read(make([]byte, 8)); err == nil {
		t.Error("expected queue-full error")
	}
}

// --- injection harness + transport-error coverage ---------------------

var errInjected = errors.New("injected transport failure")

type failPoint struct {
	method string
	nth    int // fail on this 1-based call count to method; 0 = never
}

// injectTransport wraps a fakeRngDevice and fails the nth call to a
// chosen method once `enable` is set. ReadConfig8/ReadConfig32/Read32/
// Read64 are never targeted, so they stay promoted from the embedded
// device.
type injectTransport struct {
	*fakeRngDevice
	fp       failPoint
	counts   map[string]int
	enable   bool
	zeroPhys bool
}

func newInject(d *fakeRngDevice, enable bool) *injectTransport {
	return &injectTransport{fakeRngDevice: d, counts: map[string]int{}, enable: enable}
}

func (t *injectTransport) fail(m string) bool {
	if !t.enable || t.fp.method != m {
		return false
	}
	t.counts[m]++
	return t.counts[m] == t.fp.nth
}

func (t *injectTransport) ReadConfig16(o uint8) (uint16, error) {
	if t.fail("ReadConfig16") {
		return 0, errInjected
	}
	return t.fakeRngDevice.ReadConfig16(o)
}
func (t *injectTransport) Read8(b uint8, o uint64) (uint8, error) {
	if t.fail("Read8") {
		return 0, errInjected
	}
	return t.fakeRngDevice.Read8(b, o)
}
func (t *injectTransport) Read16(b uint8, o uint64) (uint16, error) {
	if t.fail("Read16") {
		return 0, errInjected
	}
	return t.fakeRngDevice.Read16(b, o)
}
func (t *injectTransport) Write8(b uint8, o uint64, v uint8) error {
	if t.fail("Write8") {
		return errInjected
	}
	return t.fakeRngDevice.Write8(b, o, v)
}
func (t *injectTransport) Write16(b uint8, o uint64, v uint16) error {
	if t.fail("Write16") {
		return errInjected
	}
	return t.fakeRngDevice.Write16(b, o, v)
}
func (t *injectTransport) Write32(b uint8, o uint64, v uint32) error {
	if t.fail("Write32") {
		return errInjected
	}
	return t.fakeRngDevice.Write32(b, o, v)
}
func (t *injectTransport) Write64(b uint8, o uint64, v uint64) error {
	if t.fail("Write64") {
		return errInjected
	}
	return t.fakeRngDevice.Write64(b, o, v)
}
func (t *injectTransport) AllocatePages(c int) (uint64, []byte, error) {
	if t.fail("AllocatePages") {
		return 0, nil, errInjected
	}
	phys, mem, err := t.fakeRngDevice.AllocatePages(c)
	if t.enable && t.zeroPhys {
		return 0, mem, nil
	}
	return phys, mem, err
}

// TestOpenVirtioRng_TransportErrors drives every `if err != nil` return
// inside OpenVirtioRng + setupQueue by failing the corresponding
// transport call. The (method, nth) coordinates follow the fixed call
// order of the bring-up sequence.
func TestOpenVirtioRng_TransportErrors(t *testing.T) {
	cases := []struct {
		name string
		fp   failPoint
	}{
		{"DIDRead", failPoint{"ReadConfig16", 1}},
		{"InitModernConfig", failPoint{"ReadConfig16", 2}}, // PCI status read
		{"ResetStatus", failPoint{"Write8", 1}},
		{"PostResetStatusRead", failPoint{"Read8", 1}},
		{"AckStatus", failPoint{"Write8", 2}},
		{"DriverStatus", failPoint{"Write8", 3}},
		{"DeviceFeatures", failPoint{"Write32", 1}}, // WriteDeviceFeatureSelect(0)
		{"DriverFeatures", failPoint{"Write32", 3}}, // WriteDriverFeatureSelect(0)
		{"FeaturesOKStatus", failPoint{"Write8", 4}},
		{"PostFeaturesStatusRead", failPoint{"Read8", 2}},
		{"SelectQueue", failPoint{"Write16", 1}},
		{"QueueSize", failPoint{"Read16", 1}},
		{"SetQueueSize", failPoint{"Write16", 2}},
		{"QueueNotifyOff", failPoint{"Read16", 2}},
		{"AllocVirtqueue", failPoint{"AllocatePages", 1}},
		{"SetQueueDesc", failPoint{"Write64", 1}},
		{"SetQueueDriver", failPoint{"Write64", 2}},
		{"SetQueueDevice", failPoint{"Write64", 3}},
		{"SetQueueEnable", failPoint{"Write16", 3}},
		{"DriverOKStatus", failPoint{"Write8", 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeRngDevice(common.FeatureVersion1)
			it := newInject(d, true)
			it.fp = tc.fp
			if _, err := OpenVirtioRng(it); err == nil {
				t.Fatalf("%s: expected error injected at %+v", tc.name, tc.fp)
			}
		})
	}
}
