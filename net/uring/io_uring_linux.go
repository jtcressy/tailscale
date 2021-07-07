package uring

// #cgo CFLAGS: -I${SRCDIR}/liburing/src/include
// #cgo LDFLAGS: -L${SRCDIR}/liburing/src/ -luring
// #include "io_uring.c"
import "C"

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"inet.af/netaddr"
	"tailscale.com/util/endian"
)

const bufferSize = device.MaxSegmentSize

func URingAvailable() bool { return *useIOURing && C.has_io_uring() > 0 }

// A UDPConn is a recv-only UDP fd manager.
// We'd like to enqueue a bunch of recv calls and deqeueue them later,
// but we have a problem with buffer management: We get our buffers just-in-time
// from wireguard-go, which means we have to make copies.
// That's OK for now, but later it could be a performance issue.
// For now, keep it simple and enqueue/dequeue in a single step.
type UDPConn struct {
	// We have two urings so that we don't have to demux completion events.

	// recvRing is the uring for recvmsg calls.
	recvRing *C.go_uring
	// sendRing is the uring for sendmsg calls.
	sendRing *C.go_uring

	// close ensures that connection closes occur exactly once.
	close sync.Once
	// closed is an atomic variable that indicates whether the connection has been closed.
	// TODO: Make an atomic bool type that we can use here.
	closed uint32

	// local is the local address of this UDPConn.
	local net.Addr

	// recvReqs is an array of re-usable UDP recvmsg requests.
	// We attempt to keep them all queued up for the kernel to fulfill.
	// The array length is tied to the size of the uring.
	recvReqs [8]*C.goreq
	// sendReqs is an array of re-usable UDP sendmsg requests.
	// We dispatch them to the kernel as writes are requested.
	// The array length is tied to the size of the uring.
	sendReqs [8]*C.goreq

	// sendReqC is a channel containing indices into sendReqs
	// that are free to use (that is, not in the kernel).
	sendReqC chan int
	is4      bool
	// reads counts the number of outstanding read requests.
	// It is accessed atomically.
	reads int32
}

var (
	mu sync.Mutex
	// checks capabilities available on this system
	capabilities map[IORingOp]bool
)

func checkCapability(op IORingOp) bool {
	mu.Lock()
	defer mu.Unlock()
	if v, ok := capabilities[op]; ok {
		return v
	}

	has_op := C.has_capability(C.int(op)) == 1
	capabilities[op] = has_op
	return has_op
}

func NewUDPConn(pconn net.PacketConn) (*UDPConn, error) {
	if !*useIOURing {
		return nil, DisabledError
	}
	conn, ok := pconn.(*net.UDPConn)
	if !ok {
		return nil, fmt.Errorf("cannot use io_uring with conn of type %T", pconn)
	}
	// this is dumb
	local := conn.LocalAddr()
	var ipp netaddr.IPPort
	switch l := local.(type) {
	case *net.UDPAddr:
		ip, ok := netaddr.FromStdIP(l.IP)
		if !ok {
			return nil, fmt.Errorf("failed to parse IP: %v", ip)
		}
		ipp = netaddr.IPPortFrom(ip, uint16(l.Port))
	default:
		var err error
		if ipp, err = netaddr.ParseIPPort(l.String()); err != nil {
			return nil, fmt.Errorf("failed to parse UDPConn local addr %s as IP: %w", local, err)
		}
	}
	ipVersion := 6
	if ipp.IP().Is4() {
		ipVersion = 4
	}
	// TODO: probe for system capabilities: https://unixism.net/loti/tutorial/probe_liburing.html
	file, err := conn.File()
	if err != nil {
		return nil, err
	}
	// conn.File dup'd the conn's fd. We no longer need the original conn.
	conn.Close()
	recvRing := new(C.go_uring)
	sendRing := new(C.go_uring)

	fd := file.Fd()
	for _, r := range []*C.go_uring{recvRing, sendRing} {
		ret := C.initialize(r, C.int(fd))
		if ret < 0 {
			// TODO: free recvRing if sendRing initialize failed
			return nil, fmt.Errorf("uring initialization failed: %d", ret)
		}
	}
	u := &UDPConn{
		recvRing: recvRing,
		sendRing: sendRing,
		local:    conn.LocalAddr(),
		is4:      ipVersion == 4,
	}

	// Initialize buffers
	for _, reqs := range []*[8]*C.goreq{&u.recvReqs, &u.sendReqs} {
		for i := range reqs {
			reqs[i] = C.initializeReq(bufferSize, C.int(ipVersion))
		}
	}

	// Initialize recv half.
	for i := range u.recvReqs {
		if err := u.submitRecvRequest(i); err != nil {
			u.Close() // TODO: will this crash?
			return nil, err
		}
	}
	// Initialize send half.
	u.sendReqC = make(chan int, len(u.sendReqs))
	for i := range u.sendReqs {
		u.sendReqC <- i
	}
	return u, nil
}

func (u *UDPConn) submitRecvRequest(idx int) error {
	// TODO: make a C struct instead of a Go struct, and pass that in, to simplify call sites.
	errno := C.submit_recvmsg_request(u.recvRing, u.recvReqs[idx], C.size_t(idx))
	if errno < 0 {
		return fmt.Errorf("uring.submitRecvRequest failed: %w", syscall.Errno(-errno)) // TODO: Improve
	}
	atomic.AddInt32(u.recvReqInKernel(idx), 1) // TODO: CAS?
	return nil
}

func (u *UDPConn) recvReqInKernel(idx int) *int32 {
	return (*int32)(unsafe.Pointer(&u.recvReqs[idx].in_kernel))
}

// TODO: replace with unsafe.Slice once we are using Go 1.17.

func sliceOf(ptr *C.char, n int) []byte {
	var b []byte
	h := (*reflect.SliceHeader)(unsafe.Pointer(&b))
	h.Data = uintptr(unsafe.Pointer(ptr))
	h.Len = n
	h.Cap = n
	return b
}

func (u *UDPConn) ReadFromNetaddr(buf []byte) (int, netaddr.IPPort, error) {
	// Important: register that there is a read before checking whether the conn is closed.
	// Close assumes that once it has set u.closed to non-zero there are no "hidden" reads outstanding,
	// as their could be if we did this in the other order.
	atomic.AddInt32(&u.reads, 1)
	defer atomic.AddInt32(&u.reads, -1)
	if atomic.LoadUint32(&u.closed) != 0 {
		return 0, netaddr.IPPort{}, net.ErrClosed
	}
	n, idx, err := waitCompletion(u.recvRing)
	if err != nil {
		if errors.Is(err, syscall.ECANCELED) {
			atomic.AddInt32(u.recvReqInKernel(idx), -1)
		}
		// io_uring failed to run our syscall.
		return 0, netaddr.IPPort{}, fmt.Errorf("ReadFromNetaddr io_uring could not run syscall: %w", err)
	}
	atomic.AddInt32(u.recvReqInKernel(idx), -1)
	if n < 0 {
		// io_uring ran our syscall, which failed.
		// Best effort attempt not to leak idx.
		u.submitRecvRequest(int(idx))
		return 0, netaddr.IPPort{}, fmt.Errorf("ReadFromNetaddr syscall failed: %w", syscall.Errno(-n))
	}
	r := u.recvReqs[idx]
	var ip netaddr.IP
	var port uint16
	// TODO: native go endianness conversion routines so we don't have to call ntohl, etc.
	if u.is4 {
		ip = netaddr.IPFrom4(*(*[4]byte)((unsafe.Pointer)((&r.sa.sin_addr.s_addr))))
		port = endian.Ntoh16(uint16(r.sa.sin_port))
	} else {
		ip = netaddr.IPFrom16(*(*[16]byte)((unsafe.Pointer)((&r.sa6.sin6_addr))))
		port = endian.Ntoh16(uint16(r.sa6.sin6_port))
	}
	ipp := netaddr.IPPortFrom(ip, port)
	rbuf := sliceOf(r.buf, n)
	copy(buf, rbuf)
	// Queue up a new request.
	if err := u.submitRecvRequest(int(idx)); err != nil {
		// Aggressively return this error.
		// The error will bubble up and cause the entire conn to be closed down,
		// so it doesn't matter that we lost a packet here.
		return 0, netaddr.IPPort{}, err
	}
	return n, ipp, nil
}

func (u *UDPConn) Close() error {
	u.close.Do(func() {
		// Announce to readers and writers that we are closing down.
		atomic.StoreUint32(&u.closed, 1)
		// It is now not possible for u.reads to reach zero without
		// all reads being unblocked.

		// Busy loop until all reads are unblocked.
		// This is unpleasant, but I don't know of another way that
		// doesn't introduce significant synchronization overhead.
		// (The obvious alternative is to use a sync.RWMutex,
		// but that has a chicken-and-egg problem: Reads must take an rlock,
		// but we cannot take a wlock under all the rlocks are released,
		// but we cannot issue cancellations to release the rlocks without
		// first taking a wlock.)
	BusyLoop:
		for {
			for idx := range u.recvReqs {
				if atomic.LoadInt32(u.recvReqInKernel(idx)) != 0 {
					C.submit_cancel_request(u.recvRing, C.size_t(idx))
				}
			}
			reads := atomic.LoadInt32(&u.reads)
			if reads > 0 {
				time.Sleep(time.Millisecond)
			} else {
				break BusyLoop
			}
		}
		// TODO: block until no one else uses our rings.
		// (Or is that unnecessary now?)
		C.io_uring_queue_exit(u.recvRing)
		C.io_uring_queue_exit(u.sendRing)

		// Free buffers
		for _, r := range u.recvReqs {
			C.freeReq(r)
		}
		for _, r := range u.sendReqs {
			C.freeReq(r)
		}
	})
	return nil
}

// Implement net.PacketConn, for convenience integrating with magicsock.

var _ net.PacketConn = (*UDPConn)(nil)

type udpAddr struct {
	ipp netaddr.IPPort
}

func (u udpAddr) Network() string { return "udp4" } // TODO: ipv6
func (u udpAddr) String() string  { return u.ipp.String() }

func (c *UDPConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, ipp, err := c.ReadFromNetaddr(p)
	if err != nil {
		return 0, nil, err
	}
	return n, udpAddr{ipp: ipp}, err
}

func (u *UDPConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	if atomic.LoadUint32(&u.closed) != 0 {
		return 0, net.ErrClosed
	}
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, fmt.Errorf("cannot WriteTo net.Addr of type %T", addr)
	}
	// If we need a buffer, get a buffer, potentially blocking.
	var idx int
	select {
	case idx = <-u.sendReqC:
	default:
		// No request available. Get one from the kernel.
		n, idx, err = waitCompletion(u.sendRing)
		if err != nil {
			// io_uring failed to issue the syscall.
			return 0, fmt.Errorf("WriteTo io_uring call failed: %w", err)
		}
		if n < 0 {
			// Past syscall failed.
			u.sendReqC <- idx // don't leak idx
			return 0, fmt.Errorf("previous WriteTo failed: %w", syscall.Errno(-n))
		}
	}
	r := u.sendReqs[idx]
	// Do the write.
	rbuf := sliceOf(r.buf, len(p))
	copy(rbuf, p)

	if u.is4 {
		ipu32 := binary.BigEndian.Uint32(udpAddr.IP)
		r.sa.sin_addr.s_addr = C.uint32_t(endian.Hton32(ipu32))
		r.sa.sin_port = C.uint16_t(endian.Hton16(uint16(udpAddr.Port)))
		r.sa.sin_family = C.AF_INET
	} else {
		dst := (*[16]byte)((unsafe.Pointer)(&r.sa6.sin6_addr))
		src := (*[16]byte)((unsafe.Pointer)(&udpAddr.IP[0]))
		*dst = *src
		r.sa6.sin6_port = C.uint16_t(endian.Hton16(uint16(udpAddr.Port)))
		r.sa6.sin6_family = C.AF_INET6
	}
	C.submit_sendmsg_request(
		u.sendRing, // ring
		r,
		C.int(len(p)), // buffer len, ditto
		C.size_t(idx), // user data
	)
	// Get an extra buffer, if available.
	if idx, ok := peekCompletion(u.sendRing); ok {
		// Put the request buffer back in the usable queue.
		// Should never block, by construction.
		u.sendReqC <- idx
	}
	return len(p), nil
}

// LocalAddr returns the local network address.
func (c *UDPConn) LocalAddr() net.Addr { return c.local }

func (c *UDPConn) SetDeadline(t time.Time) error      { panic("not implemented") }
func (c *UDPConn) SetReadDeadline(t time.Time) error  { panic("not implemented") }
func (c *UDPConn) SetWriteDeadline(t time.Time) error { panic("not implemented") }

// Files!

// A File is a write-only file fd manager.
// TODO: Support reads
// TODO: all the todos from UDPConn
type file struct {
	writeRing *C.go_uring
	readRing  *C.go_uring
	close     sync.Once
	file      *os.File // must keep file from being GC'd
	fd        uintptr
	readReqs  [1]*C.goreq // Whoops! The kernel apparently cannot handle more than 1 concurrent preadv calls on a tun device!
	writeReqs [8]*C.goreq
	writeReqC chan int // indices into reqs
}

func newFile(f *os.File) (*file, error) {
	fd := f.Fd()
	u := &file{
		file: f,
		fd:   fd,
	}
	for _, ringPtr := range []**C.go_uring{&u.writeRing, &u.readRing} {
		r := new(C.go_uring)
		ret := C.initialize(r, C.int(fd))
		if ret < 0 {
			// TODO: handle unwinding partial initialization
			return nil, fmt.Errorf("uring initialization failed: %d", ret)
		}
		*ringPtr = r
	}

	// Initialize buffers
	for i := range &u.readReqs {
		u.readReqs[i] = C.initializeReq(bufferSize, 0)
	}
	for i := range &u.writeReqs {
		u.writeReqs[i] = C.initializeReq(bufferSize, 0)
	}

	// Initialize read half.
	for i := range u.readReqs {
		if err := u.submitReadvRequest(i); err != nil {
			u.Close() // TODO: will this crash?
			return nil, err
		}
	}

	u.writeReqC = make(chan int, len(u.writeReqs))
	for i := range u.writeReqs {
		u.writeReqC <- i
	}
	return u, nil
}

func (u *file) submitReadvRequest(idx int) error {
	// TODO: make a C struct instead of a Go struct, and pass that in, to simplify call sites.
	errno := C.submit_readv_request(u.readRing, u.readReqs[idx], C.size_t(idx))
	if errno < 0 {
		return fmt.Errorf("uring.submitReadvRequest failed: %v", errno) // TODO: Improve
	}
	return nil
}

const (
	noBlockForCompletion = 0
	blockForCompletion   = 1
)

// waitCompletion blocks until a completion on ring succeeds, or until *fd == 0.
// If *fd == 0, that indicates that the ring is no loner valid, in which case waitCompletion returns net.ErrClosed.
// Reads of *fd are atomic.
func waitCompletion(ring *C.go_uring) (n, idx int, err error) {
	for {
		r := C.completion(ring, blockForCompletion)
		if syscall.Errno(-r.err) == syscall.EAGAIN {
			continue
		}
		var err error
		if r.err < 0 {
			err = syscall.Errno(-r.err)
		}
		return int(r.n), int(r.idx), err
	}
}

func peekCompletion(ring *C.go_uring) (idx int, ok bool) {
	r := C.completion(ring, noBlockForCompletion)
	if r.err < 0 {
		return 0, false
	}
	return int(r.idx), true
}

type fileReq struct {
	iov C.go_iovec
	buf [device.MaxSegmentSize]byte
}

// Read data into buf[offset:].
// We are allowed to write junk into buf[offset-4:offset].
func (u *file) Read(buf []byte) (n int, err error) { // read a packet from the device (without any additional headers)
	if u.fd == 0 { // TODO: review all uses of u.fd for atomic read/write
		return 0, errors.New("invalid uring.File")
	}
	n, idx, err := waitCompletion(u.readRing)
	if err != nil {
		return 0, fmt.Errorf("Read: io_uring failed to issue syscall: %w", err)
	}
	if n < 0 {
		// Syscall failed.
		u.submitReadvRequest(int(idx)) // best effort attempt not to leak idx
		return 0, fmt.Errorf("Read: syscall failed: %w", syscall.Errno(-n))
	}
	// Success.
	r := u.readReqs[idx]
	rbuf := sliceOf(r.buf, n)
	copy(buf, rbuf)
	// Queue up a new request.
	if err := u.submitReadvRequest(int(idx)); err != nil {
		// Aggressively return this error.
		return 0, err
	}
	return n, nil
}

func (u *file) Write(buf []byte) (int, error) {
	if u.fd == 0 {
		return 0, errors.New("invalid uring.FileConn")
	}
	// If we need a buffer, get a buffer, potentially blocking.
	var idx int
	select {
	case idx = <-u.writeReqC:
	default:
		// No request available. Get one from the kernel.
		n, idx, err := waitCompletion(u.writeRing)
		if err != nil {
			return 0, fmt.Errorf("Write io_uring call failed: %w", err)
		}
		if n < 0 {
			// Past syscall failed.
			u.writeReqC <- idx // don't leak idx
			return 0, fmt.Errorf("previous Write failed: %w", syscall.Errno(-n))
		}
	}
	r := u.writeReqs[idx]
	// Do the write.
	rbuf := sliceOf(r.buf, len(buf))
	copy(rbuf, buf)
	C.submit_writev_request(u.writeRing, r, C.int(len(buf)), C.size_t(idx))
	// Get an extra buffer, if available.
	idx, ok := peekCompletion(u.writeRing)
	if ok {
		// Put the request buffer back in the usable queue.
		// Should never block, by construction.
		u.writeReqC <- idx
	}
	return len(buf), nil
}

func (u *file) Close() error {
	u.close.Do(func() {
		atomic.StoreUintptr(&u.fd, 0)
		u.file.Close()
		u.file = nil
		// TODO: bring the shutdown logic from UDPConn.Close here?
		// Or is closing the file above enough, unlike for UDP?
		C.io_uring_queue_exit(u.readRing)
		C.io_uring_queue_exit(u.writeRing)

		// Free buffers
		for _, r := range u.readReqs {
			C.freeReq(r)
		}
		for _, r := range u.writeReqs {
			C.freeReq(r)
		}
	})
	return nil
}

// Wrap files into TUN devices.

func NewTUN(d tun.Device) (tun.Device, error) {
	nt, ok := d.(*tun.NativeTun)
	if !ok {
		return nil, fmt.Errorf("NewTUN only wraps *tun.NativeTun, got %T", d)
	}
	f, err := newFile(nt.File())
	if err != nil {
		return nil, err
	}
	v := reflect.ValueOf(nt)
	field, ok := v.Elem().Type().FieldByName("errors")
	if !ok {
		return nil, errors.New("could not find internal tun.NativeTun errors field")
	}
	ptr := unsafe.Pointer(nt)
	ptr = unsafe.Pointer(uintptr(ptr) + field.Offset) // TODO: switch to unsafe.Add with Go 1.17...as if that's the worst thing in this line
	c := *(*chan error)(ptr)
	return &TUN{d: nt, f: f, errors: c}, nil
}

// No nopi
type TUN struct {
	d      *tun.NativeTun
	f      *file
	errors chan error
}

func (t *TUN) File() *os.File {
	return t.f.file
}

func (t *TUN) Read(buf []byte, offset int) (int, error) {
	select {
	case err := <-t.errors:
		return 0, err
	default:
	}
	// TODO: upstream has graceful shutdown error handling here.
	buff := buf[offset-4:]
	n, err := t.f.Read(buff[:])
	if errors.Is(err, syscall.EBADFD) {
		err = os.ErrClosed
	}
	if n < 4 {
		n = 0
	} else {
		n -= 4
	}
	return n, err
}

func (t *TUN) Write(buf []byte, offset int) (int, error) {
	// below copied from wireguard-go NativeTun.Write

	// reserve space for header
	buf = buf[offset-4:]

	// add packet information header
	buf[0] = 0x00
	buf[1] = 0x00
	if buf[4]>>4 == ipv6.Version {
		buf[2] = 0x86
		buf[3] = 0xdd
	} else {
		buf[2] = 0x08
		buf[3] = 0x00
	}

	n, err := t.f.Write(buf)
	if errors.Is(err, syscall.EBADFD) {
		err = os.ErrClosed
	}
	return n, err
}

func (t *TUN) Flush() error           { return t.d.Flush() }
func (t *TUN) MTU() (int, error)      { return t.d.MTU() }
func (t *TUN) Name() (string, error)  { return t.d.Name() }
func (t *TUN) Events() chan tun.Event { return t.d.Events() }

func (t *TUN) Close() error {
	err1 := t.f.Close()
	err2 := t.d.Close()
	if err1 != nil {
		return err1
	}
	return err2
}