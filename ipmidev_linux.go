//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// The kernel IPMI interface (include/uapi/linux/ipmi.h), as laid out on
// 64-bit Linux. The padding is explicit so the layout can be checked in a
// test against the C sizes and offsets.

const (
	ipmiSystemInterfaceAddrType = 0x0c // IPMI_SYSTEM_INTERFACE_ADDR_TYPE
	ipmiBMCChannel              = 0x0f // IPMI_BMC_CHANNEL
	ipmiResponseRecvType        = 1    // IPMI_RESPONSE_RECV_TYPE
	ipmiMaxAddrSize             = 32   // IPMI_MAX_ADDR_SIZE
	ipmiMaxMsgLength            = 272  // IPMI_MAX_MSG_LENGTH
)

// struct ipmi_system_interface_addr
type ipmiSystemInterfaceAddr struct {
	addrType int32
	channel  int16
	lun      uint8
	_        uint8
}

// struct ipmi_addr: big enough for any address the kernel reports back.
type ipmiAddr struct {
	addrType int32
	channel  int16
	data     [ipmiMaxAddrSize]byte
	_        [2]byte
}

// struct ipmi_msg
type ipmiMsg struct {
	netfn   uint8
	cmd     uint8
	dataLen uint16
	_       [4]byte
	data    unsafe.Pointer
}

// struct ipmi_req
type ipmiReq struct {
	addr    unsafe.Pointer
	addrLen uint32
	_       [4]byte
	msgid   int64
	msg     ipmiMsg
}

// struct ipmi_recv
type ipmiRecv struct {
	recvType int32
	_        [4]byte
	addr     unsafe.Pointer
	addrLen  uint32
	_        [4]byte
	msgid    int64
	msg      ipmiMsg
}

// Linux ioctl number encoding (asm-generic/ioctl.h, used by amd64 and arm64).
func ioc(dir, typ, nr, size uintptr) uintptr { return dir<<30 | size<<16 | typ<<8 | nr }

const (
	iocWrite = 1
	iocRead  = 2
)

var (
	// _IOR('i', 13, struct ipmi_req). The kernel says "read" for a send.
	ipmictlSendCommand = ioc(iocRead, 'i', 13, unsafe.Sizeof(ipmiReq{}))
	// _IOWR('i', 11, struct ipmi_recv)
	ipmictlReceiveMsgTrunc = ioc(iocRead|iocWrite, 'i', 11, unsafe.Sizeof(ipmiRecv{}))
)

// ipmiPollSlice bounds each wait for the response, so a cancelled context
// is noticed promptly.
const ipmiPollSlice = 100 * time.Millisecond

// ipmiDevice is the transport that talks to the BMC through /dev/ipmi0,
// opened once. Each request waits at most ipmiTimeout for its response.
type ipmiDevice struct {
	path    string
	fd      int
	timeout time.Duration

	// Swapped out in tests, so no real device is needed.
	ioctl func(fd int, req uintptr, arg unsafe.Pointer) syscall.Errno
	wait  func(fd int, timeout time.Duration) (readable bool, err error)

	mu    sync.Mutex // one request/response at a time
	msgid int64
}

// openIPMIDevice opens the kernel IPMI device, explaining the usual reasons
// it isn't there.
func openIPMIDevice(path string) (*ipmiDevice, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, syscall.ENOENT):
		return nil, fmt.Errorf("open %s: %w; load the IPMI driver with `modprobe ipmi_si ipmi_devintf`, or set \"ipmi\": \"ipmitool\" in the config", path, err)
	case errors.Is(err, syscall.EACCES):
		return nil, fmt.Errorf("open %s: %w; fanctl must run as root", path, err)
	case err != nil:
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &ipmiDevice{path: path, fd: fd, timeout: ipmiTimeout, ioctl: sysIoctl, wait: pollReadable}, nil
}

func (d *ipmiDevice) name() string { return d.path }

func (d *ipmiDevice) Close() error { return syscall.Close(d.fd) }

func (d *ipmiDevice) raw(ctx context.Context, netfn, cmd byte, data []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.msgid++
	id := d.msgid

	// The kernel reads these through raw pointers: pin them so they stay
	// put, and alive, until it is done.
	var pin runtime.Pinner
	defer pin.Unpin()

	addr := &ipmiSystemInterfaceAddr{addrType: ipmiSystemInterfaceAddrType, channel: ipmiBMCChannel}
	req := &ipmiReq{
		addr:    unsafe.Pointer(addr),
		addrLen: uint32(unsafe.Sizeof(*addr)),
		msgid:   id,
		msg:     ipmiMsg{netfn: netfn, cmd: cmd, dataLen: uint16(len(data))},
	}
	pin.Pin(addr)
	pin.Pin(req)
	if len(data) > 0 {
		buf := append([]byte(nil), data...)
		pin.Pin(&buf[0])
		req.msg.data = unsafe.Pointer(&buf[0])
	}
	if err := d.retry(ipmictlSendCommand, unsafe.Pointer(req)); err != nil {
		return fmt.Errorf("send: %w", err)
	}

	from := &ipmiAddr{}
	resp := make([]byte, ipmiMaxMsgLength)
	recv := &ipmiRecv{}
	pin.Pin(from)
	pin.Pin(&resp[0])
	pin.Pin(recv)

	deadline := time.Now().Add(d.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("no response within %s", d.timeout)
		}
		ready, err := d.wait(d.fd, min(left, ipmiPollSlice))
		if err != nil {
			return fmt.Errorf("poll: %w", err)
		}
		if !ready {
			continue
		}
		*recv = ipmiRecv{
			addr:    unsafe.Pointer(from),
			addrLen: uint32(unsafe.Sizeof(*from)),
			msg:     ipmiMsg{dataLen: uint16(len(resp)), data: unsafe.Pointer(&resp[0])},
		}
		err = d.retry(ipmictlReceiveMsgTrunc, unsafe.Pointer(recv))
		switch {
		case errors.Is(err, syscall.EAGAIN):
			continue // woken, but nothing queued after all
		case errors.Is(err, syscall.EMSGSIZE):
			// Truncated to our buffer; the completion code is still byte 0.
		case err != nil:
			return fmt.Errorf("receive: %w", err)
		}
		if recv.recvType != ipmiResponseRecvType || recv.msgid != id {
			continue // a stray message, such as a late reply to a request that timed out
		}
		if recv.msg.dataLen == 0 {
			return errors.New("empty response")
		}
		if cc := resp[0]; cc != 0 {
			return completionError(cc)
		}
		return nil
	}
}

// retry runs one ioctl, again if a signal interrupts it.
func (d *ipmiDevice) retry(req uintptr, arg unsafe.Pointer) error {
	for {
		switch errno := d.ioctl(d.fd, req, arg); errno {
		case 0:
			return nil
		case syscall.EINTR:
		default:
			return errno
		}
	}
}

func sysIoctl(fd int, req uintptr, arg unsafe.Pointer) syscall.Errno {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	return errno
}

// struct pollfd
type pollFd struct {
	fd      int32
	events  int16
	revents int16
}

const (
	pollIn   = 0x1
	pollErr  = 0x8
	pollHup  = 0x10
	pollNval = 0x20
)

// pollReadable waits up to timeout for fd to have a message. ppoll, not
// poll: arm64 has no poll syscall.
func pollReadable(fd int, timeout time.Duration) (bool, error) {
	p := pollFd{fd: int32(fd), events: pollIn}
	ts := syscall.NsecToTimespec(int64(timeout))
	n, _, errno := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&p)), 1, uintptr(unsafe.Pointer(&ts)), 0, 0, 0)
	switch {
	case errno == syscall.EINTR:
		return false, nil // the caller loops
	case errno != 0:
		return false, errno
	case n == 0:
		return false, nil
	case p.revents&(pollErr|pollHup|pollNval) != 0:
		return false, fmt.Errorf("device error (revents 0x%x)", p.revents)
	}
	return p.revents&pollIn != 0, nil
}
