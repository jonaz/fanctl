//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// The structs must match the C layout in include/uapi/linux/ipmi.h on
// 64-bit Linux, or the kernel reads garbage.
func TestIPMIStructLayout(t *testing.T) {
	var req ipmiReq
	var recv ipmiRecv
	var msg ipmiMsg
	var addr ipmiSystemInterfaceAddr
	for _, c := range []struct {
		what      string
		got, want uintptr
	}{
		{"sizeof ipmi_system_interface_addr", unsafe.Sizeof(addr), 8},
		{"sizeof ipmi_addr", unsafe.Sizeof(ipmiAddr{}), 40},
		{"sizeof ipmi_msg", unsafe.Sizeof(msg), 16},
		{"ipmi_msg.data_len", unsafe.Offsetof(msg.dataLen), 2},
		{"ipmi_msg.data", unsafe.Offsetof(msg.data), 8},
		{"sizeof ipmi_req", unsafe.Sizeof(req), 40},
		{"ipmi_req.addr_len", unsafe.Offsetof(req.addrLen), 8},
		{"ipmi_req.msgid", unsafe.Offsetof(req.msgid), 16},
		{"ipmi_req.msg", unsafe.Offsetof(req.msg), 24},
		{"sizeof ipmi_recv", unsafe.Sizeof(recv), 48},
		{"ipmi_recv.addr", unsafe.Offsetof(recv.addr), 8},
		{"ipmi_recv.addr_len", unsafe.Offsetof(recv.addrLen), 16},
		{"ipmi_recv.msgid", unsafe.Offsetof(recv.msgid), 24},
		{"ipmi_recv.msg", unsafe.Offsetof(recv.msg), 32},
		{"ipmi_system_interface_addr.channel", unsafe.Offsetof(addr.channel), 4},
		{"ipmi_system_interface_addr.lun", unsafe.Offsetof(addr.lun), 6},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.what, c.got, c.want)
		}
	}
}

func TestIPMIIoctlNumbers(t *testing.T) {
	if ipmictlSendCommand != 0x8028690d {
		t.Errorf("IPMICTL_SEND_COMMAND = %#x, want 0x8028690d", ipmictlSendCommand)
	}
	if ipmictlReceiveMsgTrunc != 0xc030690b {
		t.Errorf("IPMICTL_RECEIVE_MSG_TRUNC = %#x, want 0xc030690b", ipmictlReceiveMsgTrunc)
	}
}

// fakeKernel stands in for the IPMI driver behind the ioctls.
type fakeKernel struct {
	sent []sentReq
	// reply builds what is queued for each request; nil = no reply.
	reply func(r sentReq) []queued
	queue []queued
}

type sentReq struct {
	addrType int32
	channel  int16
	lun      uint8
	addrLen  uint32
	msgid    int64
	netfn    uint8
	cmd      uint8
	data     []byte
}

type queued struct {
	recvType int32
	msgid    int64
	data     []byte
}

func (k *fakeKernel) ioctl(_ int, req uintptr, arg unsafe.Pointer) syscall.Errno {
	switch req {
	case ipmictlSendCommand:
		r := (*ipmiReq)(arg)
		a := (*ipmiSystemInterfaceAddr)(r.addr)
		s := sentReq{
			addrType: a.addrType, channel: a.channel, lun: a.lun, addrLen: r.addrLen,
			msgid: r.msgid, netfn: r.msg.netfn, cmd: r.msg.cmd,
			data: append([]byte(nil), unsafe.Slice((*byte)(r.msg.data), r.msg.dataLen)...),
		}
		k.sent = append(k.sent, s)
		if k.reply != nil {
			k.queue = append(k.queue, k.reply(s)...)
		}
		return 0
	case ipmictlReceiveMsgTrunc:
		if len(k.queue) == 0 {
			return syscall.EAGAIN
		}
		q := k.queue[0]
		k.queue = k.queue[1:]
		r := (*ipmiRecv)(arg)
		if r.addrLen < uint32(unsafe.Sizeof(ipmiAddr{})) {
			return syscall.EINVAL
		}
		r.recvType, r.msgid = q.recvType, q.msgid
		n := copy(unsafe.Slice((*byte)(r.msg.data), r.msg.dataLen), q.data)
		r.msg.dataLen = uint16(n)
		return 0
	}
	return syscall.ENOTTY
}

func (k *fakeKernel) wait(_ int, timeout time.Duration) (bool, error) {
	if len(k.queue) > 0 {
		return true, nil
	}
	time.Sleep(timeout) // nothing will arrive: let the (virtual) clock run
	return false, nil
}

func fakeDevice(k *fakeKernel) *ipmiDevice {
	return &ipmiDevice{path: "/dev/ipmi0", timeout: ipmiTimeout, ioctl: k.ioctl, wait: k.wait}
}

// answer replies to every request with completion code cc.
func answer(cc byte) func(sentReq) []queued {
	return func(r sentReq) []queued {
		return []queued{{recvType: ipmiResponseRecvType, msgid: r.msgid, data: []byte{cc}}}
	}
}

func TestIPMIDeviceEncodesRequests(t *testing.T) {
	k := &fakeKernel{reply: answer(0)}
	ipmi := newIPMI(fakeDevice(k), false)
	ctx := context.Background()
	if err := ipmi.SetMode(ctx, fanModes["full"]); err != nil {
		t.Fatal(err)
	}
	if err := ipmi.SetDuty(ctx, 1, 83); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		netfn, cmd uint8
		data       []byte
	}{
		{0x30, 0x45, []byte{0x01, 0x01}},
		{0x30, 0x70, []byte{0x66, 0x01, 0x01, 83}},
	}
	if len(k.sent) != len(want) {
		t.Fatalf("%d requests sent, want %d", len(k.sent), len(want))
	}
	for i, s := range k.sent {
		if s.addrType != 0x0c || s.channel != 0x0f || s.lun != 0 || s.addrLen != 8 {
			t.Errorf("request %d: address %+v, want system interface, BMC channel", i, s)
		}
		if s.netfn != want[i].netfn || s.cmd != want[i].cmd || string(s.data) != string(want[i].data) {
			t.Errorf("request %d: netfn %#x cmd %#x data % x, want %#x %#x % x",
				i, s.netfn, s.cmd, s.data, want[i].netfn, want[i].cmd, want[i].data)
		}
	}
	if k.sent[0].msgid == k.sent[1].msgid {
		t.Errorf("both requests used msgid %d", k.sent[0].msgid)
	}
}

func TestIPMIDeviceCompletionCode(t *testing.T) {
	k := &fakeKernel{reply: answer(0xc1)}
	err := newIPMI(fakeDevice(k), false).SetDuty(context.Background(), 1, 50)
	var cc completionError
	if !errors.As(err, &cc) || cc != 0xc1 {
		t.Fatalf("got %v, want completion code 0xc1", err)
	}
	if want := "/dev/ipmi0 raw 0x30 0x70 0x66 0x01 0x01 0x32: completion code 0xc1 (invalid command)"; err.Error() != want {
		t.Fatalf("error %q, want %q", err, want)
	}
	if got := completionError(0x80).Error(); got != "completion code 0x80" {
		t.Fatalf("unknown code: %q", got)
	}
}

func TestIPMIDeviceDropsStrayMessages(t *testing.T) {
	k := &fakeKernel{reply: func(r sentReq) []queued {
		return []queued{
			{recvType: ipmiResponseRecvType, msgid: r.msgid - 1, data: []byte{0xc3}}, // late reply to an earlier request
			{recvType: 2, msgid: r.msgid, data: []byte{0xff}},                        // not a response
			{recvType: ipmiResponseRecvType, msgid: r.msgid, data: []byte{0x00}},
		}
	}}
	if err := newIPMI(fakeDevice(k), false).SetDuty(context.Background(), 0, 40); err != nil {
		t.Fatalf("stray messages not dropped: %v", err)
	}
	if len(k.queue) != 0 {
		t.Fatalf("%d messages left unread", len(k.queue))
	}
}

func TestIPMIDeviceEmptyResponse(t *testing.T) {
	k := &fakeKernel{reply: func(r sentReq) []queued {
		return []queued{{recvType: ipmiResponseRecvType, msgid: r.msgid}}
	}}
	if err := newIPMI(fakeDevice(k), false).SetDuty(context.Background(), 0, 40); err == nil || !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("got %v, want empty response error", err)
	}
}

func TestIPMIDeviceTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := &fakeKernel{} // the BMC never answers
		start := time.Now()
		err := newIPMI(fakeDevice(k), false).SetDuty(context.Background(), 1, 50)
		if err == nil || !strings.Contains(err.Error(), "no response within 5s") {
			t.Fatalf("got %v, want timeout", err)
		}
		if took := time.Since(start); took != ipmiTimeout {
			t.Fatalf("gave up after %s, want %s", took, ipmiTimeout)
		}
	})
}

func TestIPMIDeviceHonoursContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := &fakeKernel{}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := newIPMI(fakeDevice(k), false).SetMode(ctx, fanModes["optimal"])
		if err == nil {
			t.Fatal("no error")
		}
		if took := time.Since(start); took > 250*time.Millisecond+ipmiPollSlice {
			t.Fatalf("gave up after %s, want about the context's 250ms", took)
		}
	})
}

// A cancelled call never reaches the BMC, even though the lock is free and
// select may pick it over the cancellation.
func TestCancelledCallSendsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	k := &fakeKernel{reply: answer(0)}
	f := &fakeRunner{}
	for _, tr := range []transport{fakeDevice(k), ipmitool{run: f, bin: "ipmitool"}} {
		ipmi := newIPMI(tr, false)
		for range 50 { // select's choice is random: give it every chance
			if err := ipmi.SetDuty(ctx, 1, 50); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s: got %v, want context.Canceled", tr.name(), err)
			}
		}
	}
	if len(k.sent) != 0 || f.n() != 0 {
		t.Fatalf("cancelled calls sent %d device requests and %d ipmitool commands", len(k.sent), f.n())
	}
}

func TestIPMIDeviceRetriesEINTR(t *testing.T) {
	k := &fakeKernel{reply: answer(0)}
	interrupted := 0
	d := fakeDevice(k)
	d.ioctl = func(fd int, req uintptr, arg unsafe.Pointer) syscall.Errno {
		if interrupted < 2 {
			interrupted++
			return syscall.EINTR
		}
		return k.ioctl(fd, req, arg)
	}
	if err := newIPMI(d, false).SetDuty(context.Background(), 0, 40); err != nil {
		t.Fatal(err)
	}
	if len(k.sent) != 1 {
		t.Fatalf("%d requests sent, want 1", len(k.sent))
	}
}

func TestOpenIPMIDeviceMissing(t *testing.T) {
	_, err := openIPMIDevice(t.TempDir() + "/ipmi0")
	if err == nil || !strings.Contains(err.Error(), "modprobe -a ipmi_si ipmi_devintf") || !strings.Contains(err.Error(), `"ipmi": "ipmitool"`) {
		t.Fatalf("got %v, want a hint to load the modules or use ipmitool", err)
	}
}
