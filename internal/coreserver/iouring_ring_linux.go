// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux

package coreserver

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux io_uring uapi (linux/io_uring.h).  Layout must match the kernel.
const (
	ioringOffSqRing = 0
	ioringOffCqRing = 0x8000000
	ioringOffSqes   = 0x10000000

	ioringOpAccept      = 13
	ioringOpAsyncCancel = 14
	ioringOpClose       = 19
	ioringOpRead        = 22
	ioringOpWrite       = 23
	ioringOpSend        = 26
	ioringOpRecv        = 27
	ioringOpSplice      = 30

	iosqeIoLink = 1 << 2

	ioringEnterGetEvents = 1 << 0
)

type ioUringParams struct {
	SqEntries    uint32
	CqEntries    uint32
	Flags        uint32
	SqThreadCPU  uint32
	SqThreadIdle uint32
	Features     uint32
	WqFd         uint32
	Resv         [3]uint32
	SqOff        ioUringSQRingOffsets
	CqOff        ioUringCQRingOffsets
}

type ioUringSQRingOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Flags       uint32
	Dropped     uint32
	Array       uint32
	Resv1       uint32
	UserAddr    uint64
}

type ioUringCQRingOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Overflow    uint32
	Cqes        uint32
	Flags       uint32
	Resv1       uint32
	UserAddr    uint64
}

// ioUringSqe is the kernel's 64-byte submission queue entry (layout is
// fixed in uapi/linux/io_uring.h across arches).
type ioUringSqe [64]byte

type ioUringCqe struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

const (
	sqeUserDataHdr  = 1
	sqeUserDataBody = 2

	sqeOffOpcode     = 0
	sqeOffFlags      = 1
	sqeOffFd         = 4
	sqeOffOff        = 8
	sqeOffAddr       = 16
	sqeOffLen        = 24
	sqeOffRwFlags    = 28 // accept_flags / msg_flags / cancel_flags union slot
	sqeOffUserData   = 32
	sqeOffSpliceFdIn = 44
)

func (sqe *ioUringSqe) clear() { *sqe = ioUringSqe{} }

func (sqe *ioUringSqe) setOpcode(op uint8) { sqe[sqeOffOpcode] = op }

func (sqe *ioUringSqe) setFlags(f uint8) { sqe[sqeOffFlags] = f }

func (sqe *ioUringSqe) setFd(fd int32) {
	*(*int32)(unsafe.Pointer(&sqe[sqeOffFd])) = fd
}

func (sqe *ioUringSqe) setOff(off int64) {
	*(*int64)(unsafe.Pointer(&sqe[sqeOffOff])) = off
}

func (sqe *ioUringSqe) setAddr(addr uint64) {
	*(*uint64)(unsafe.Pointer(&sqe[sqeOffAddr])) = addr
}

func (sqe *ioUringSqe) setLen(n uint32) {
	*(*uint32)(unsafe.Pointer(&sqe[sqeOffLen])) = n
}

func (sqe *ioUringSqe) setUserData(ud uint64) {
	*(*uint64)(unsafe.Pointer(&sqe[sqeOffUserData])) = ud
}

func (sqe *ioUringSqe) setSpliceFdIn(fd int32) {
	*(*int32)(unsafe.Pointer(&sqe[sqeOffSpliceFdIn])) = fd
}

// setRwFlags writes to the 32-bit union slot used by accept_flags,
// msg_flags, cancel_flags and friends.
func (sqe *ioUringSqe) setRwFlags(f uint32) {
	*(*uint32)(unsafe.Pointer(&sqe[sqeOffRwFlags])) = f
}

func init() {
	if unsafe.Sizeof(ioUringSqe{}) != 64 {
		panic(fmt.Sprintf("ioUringSqe size %d != 64", unsafe.Sizeof(ioUringSqe{})))
	}
	if unsafe.Sizeof(ioUringCqe{}) != 16 {
		panic(fmt.Sprintf("ioUringCqe size %d != 16", unsafe.Sizeof(ioUringCqe{})))
	}
}

// ioUringRing is a minimal single-issuer ring for linked WRITE+SPLICE.
// All submissions are serialized by the owner (Server.iouMu).
type ioUringRing struct {
	fd     int
	sqMask uint32
	cqMask uint32

	sqKHead  *uint32
	sqKTail  *uint32
	sqKFlags *uint32
	cqKHead  *uint32
	cqKTail  *uint32
	cqKFlags *uint32

	// sqArray is the SQ-ring indirection table.  io_uring requires
	// userspace to write sqArray[tail&mask] with the index of the SQE
	// to run before bumping sq_tail.  We initialise it to identity
	// (sqArray[i] = i) once at setup so every submission slot maps
	// onto its corresponding sqes[i] without per-call indirection
	// writes.  See io_get_sqe() in fs/io_uring.c in the kernel tree.
	sqArray []uint32

	sqes []ioUringSqe
	cqes []ioUringCqe

	sqMmap  []byte
	cqMmap  []byte
	sqeMmap []byte
}

func newIoUringRing(entries uint32, setupFlags uint32) (*ioUringRing, error) {
	if entries == 0 || entries&(entries-1) != 0 {
		return nil, fmt.Errorf("io_uring entries must be a power of 2, got %d", entries)
	}
	var params ioUringParams
	params.SqEntries = entries
	params.Flags = setupFlags
	fd, _, errno := syscall.Syscall(sysIoUringSetup, uintptr(entries), uintptr(unsafe.Pointer(&params)), 0)
	if errno != 0 {
		return nil, errno
	}
	ring := &ioUringRing{fd: int(fd), sqMask: entries - 1, cqMask: params.CqEntries - 1}

	// sqLen must reach past sqArray (params.SqOff.Array + entries*4
	// bytes) so userspace can both initialise and update the
	// indirection table.
	sqLen := int(params.SqOff.Array) + int(entries)*int(unsafe.Sizeof(uint32(0)))
	cqLen := int(params.CqOff.Cqes) + int(params.CqEntries)*int(unsafe.Sizeof(ioUringCqe{}))
	sqeLen := int(entries) * int(unsafe.Sizeof(ioUringSqe{}))

	var err error
	ring.sqMmap, err = unix.Mmap(ring.fd, ioringOffSqRing, sqLen, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		_ = unix.Close(ring.fd)
		return nil, err
	}
	ring.cqMmap, err = unix.Mmap(ring.fd, ioringOffCqRing, cqLen, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		_ = unix.Munmap(ring.sqMmap)
		_ = unix.Close(ring.fd)
		return nil, err
	}
	ring.sqeMmap, err = unix.Mmap(ring.fd, ioringOffSqes, sqeLen, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		_ = unix.Munmap(ring.cqMmap)
		_ = unix.Munmap(ring.sqMmap)
		_ = unix.Close(ring.fd)
		return nil, err
	}
	ring.sqes = unsafe.Slice((*ioUringSqe)(unsafe.Pointer(&ring.sqeMmap[0])), int(entries))
	ring.cqes = unsafe.Slice(
		(*ioUringCqe)(unsafe.Pointer(&ring.cqMmap[params.CqOff.Cqes])),
		int(params.CqEntries),
	)

	ring.sqKHead = (*uint32)(unsafe.Pointer(&ring.sqMmap[params.SqOff.Head]))
	ring.sqKTail = (*uint32)(unsafe.Pointer(&ring.sqMmap[params.SqOff.Tail]))
	ring.sqKFlags = (*uint32)(unsafe.Pointer(&ring.sqMmap[params.SqOff.Flags]))
	ring.cqKHead = (*uint32)(unsafe.Pointer(&ring.cqMmap[params.CqOff.Head]))
	ring.cqKTail = (*uint32)(unsafe.Pointer(&ring.cqMmap[params.CqOff.Tail]))
	ring.cqKFlags = (*uint32)(unsafe.Pointer(&ring.cqMmap[params.CqOff.Flags]))

	// SQ array starts at sqMmap+SqOff.Array and holds `entries`
	// u32 indices.  Initialise to identity once so userspace
	// submissions don't need to maintain the table per-SQE.
	ring.sqArray = unsafe.Slice(
		(*uint32)(unsafe.Pointer(&ring.sqMmap[params.SqOff.Array])),
		int(entries),
	)
	for i := uint32(0); i < entries; i++ {
		ring.sqArray[i] = i
	}

	return ring, nil
}

func (r *ioUringRing) close() {
	if r.fd >= 0 {
		_ = unix.Close(r.fd)
		r.fd = -1
	}
	if len(r.sqMmap) > 0 {
		_ = unix.Munmap(r.sqMmap)
		r.sqMmap = nil
	}
	if len(r.cqMmap) > 0 {
		_ = unix.Munmap(r.cqMmap)
		r.cqMmap = nil
	}
	if len(r.sqeMmap) > 0 {
		_ = unix.Munmap(r.sqeMmap)
		r.sqeMmap = nil
	}
}

func (r *ioUringRing) sqSpace() uint32 {
	head := atomic.LoadUint32(r.sqKHead)
	tail := atomic.LoadUint32(r.sqKTail)
	return r.sqMask + 1 - (tail - head)
}

func (r *ioUringRing) prepSqe() (*ioUringSqe, error) {
	if r.sqSpace() < 1 {
		return nil, errors.New("io_uring SQ full")
	}
	tail := atomic.LoadUint32(r.sqKTail)
	sqe := &r.sqes[tail&r.sqMask]
	sqe.clear()
	return sqe, nil
}

func (r *ioUringRing) sqCommit() {
	tail := atomic.LoadUint32(r.sqKTail) + 1
	atomic.StoreUint32(r.sqKTail, tail)
}

// prepAccept queues an IORING_OP_ACCEPT SQE on listenerFd. addr=0
// (don't return peer addr).  The SQE is not yet visible to the kernel
// until the next ring.enter().
//
// We pass only SOCK_CLOEXEC, NOT SOCK_NONBLOCK.  The accepted socket
// stays in default blocking mode so that the reactor's inline
// unix.Sendfile() call after the header WRITE CQE blocks in-kernel
// until the body drains, without us having to wire an io_uring
// poll loop just to absorb EAGAIN.  io_uring's RECV/WRITE submissions
// are independent of the underlying fd's O_NONBLOCK flag.
func (r *ioUringRing) prepAccept(listenerFd int32, userData uint64) error {
	sqe, err := r.prepSqe()
	if err != nil {
		return err
	}
	sqe.setOpcode(ioringOpAccept)
	sqe.setFd(listenerFd)
	sqe.setAddr(0)
	sqe.setOff(0)
	sqe.setLen(0)
	sqe.setRwFlags(uint32(unix.SOCK_CLOEXEC))
	sqe.setUserData(userData)
	r.sqCommit()
	return nil
}

// prepRecv queues an IORING_OP_RECV SQE that fills buf from connFd.
func (r *ioUringRing) prepRecv(connFd int32, buf []byte, userData uint64) error {
	if len(buf) == 0 {
		return errors.New("io_uring: empty recv buf")
	}
	sqe, err := r.prepSqe()
	if err != nil {
		return err
	}
	sqe.setOpcode(ioringOpRecv)
	sqe.setFd(connFd)
	sqe.setAddr(uint64(uintptr(unsafe.Pointer(&buf[0]))))
	sqe.setLen(uint32(len(buf)))
	sqe.setOff(0)
	sqe.setRwFlags(0)
	sqe.setUserData(userData)
	r.sqCommit()
	return nil
}

// prepSend queues an IORING_OP_SEND SQE writing buf to connFd.
func (r *ioUringRing) prepSend(connFd int32, buf []byte, userData uint64) error {
	if len(buf) == 0 {
		return errors.New("io_uring: empty send buf")
	}
	sqe, err := r.prepSqe()
	if err != nil {
		return err
	}
	sqe.setOpcode(ioringOpSend)
	sqe.setFd(connFd)
	sqe.setAddr(uint64(uintptr(unsafe.Pointer(&buf[0]))))
	sqe.setLen(uint32(len(buf)))
	sqe.setOff(0)
	sqe.setRwFlags(0)
	sqe.setUserData(userData)
	r.sqCommit()
	return nil
}

// prepRead queues an IORING_OP_READ SQE that fills buf from fd.
// Used by the reactor's eventfd wakeup mechanism: the reactor
// arms an 8-byte READ on its wakeup eventfd, and Server.Close()
// pokes the eventfd to complete the SQE and bring the reactor
// out of its blocking ring.enter() call.  See ioReactor.run /
// ioReactor.close for the full handshake.
func (r *ioUringRing) prepRead(fd int32, buf []byte, userData uint64) error {
	if len(buf) == 0 {
		return errors.New("io_uring: empty read buf")
	}
	sqe, err := r.prepSqe()
	if err != nil {
		return err
	}
	sqe.setOpcode(ioringOpRead)
	sqe.setFd(fd)
	sqe.setAddr(uint64(uintptr(unsafe.Pointer(&buf[0]))))
	sqe.setLen(uint32(len(buf)))
	sqe.setOff(0)
	sqe.setRwFlags(0)
	sqe.setUserData(userData)
	r.sqCommit()
	return nil
}

// prepClose queues an IORING_OP_CLOSE SQE for fd.
func (r *ioUringRing) prepClose(fd int32, userData uint64) error {
	sqe, err := r.prepSqe()
	if err != nil {
		return err
	}
	sqe.setOpcode(ioringOpClose)
	sqe.setFd(fd)
	sqe.setUserData(userData)
	r.sqCommit()
	return nil
}

// sqPending returns the number of SQEs queued since the last enter().
func (r *ioUringRing) sqPending() uint32 {
	head := atomic.LoadUint32(r.sqKHead)
	tail := atomic.LoadUint32(r.sqKTail)
	return tail - head
}

// drainCQ pulls all ready CQEs and calls fn for each.  Returns the
// number of CQEs drained.
func (r *ioUringRing) drainCQ(fn func(userData uint64, res int32, flags uint32)) int {
	n := 0
	for {
		head := atomic.LoadUint32(r.cqKHead)
		tail := atomic.LoadUint32(r.cqKTail)
		if head == tail {
			return n
		}
		for head != tail {
			cqe := &r.cqes[head&r.cqMask]
			ud := cqe.UserData
			res := cqe.Res
			fl := cqe.Flags
			head++
			fn(ud, res, fl)
			n++
		}
		atomic.StoreUint32(r.cqKHead, head)
	}
}

// enterNoIntr is enter() but treats EAGAIN as a non-error (no events
// were available; caller should fall through and try again on its own
// schedule).  Returns true if EAGAIN was observed.
func (r *ioUringRing) enterNoIntr(toSubmit, minComplete uint32) (eagain bool, err error) {
	for {
		_, _, errno := syscall.Syscall6(
			sysIoUringEnter,
			uintptr(r.fd),
			uintptr(toSubmit),
			uintptr(minComplete),
			uintptr(ioringEnterGetEvents),
			0, 0,
		)
		switch errno {
		case 0:
			return false, nil
		case syscall.EINTR:
			continue
		case syscall.EAGAIN:
			return true, nil
		default:
			return false, errno
		}
	}
}

func (r *ioUringRing) enter(toSubmit, minComplete uint32) error {
	for {
		_, _, errno := syscall.Syscall6(
			sysIoUringEnter,
			uintptr(r.fd),
			uintptr(toSubmit),
			uintptr(minComplete),
			uintptr(ioringEnterGetEvents),
			0, 0,
		)
		switch errno {
		case 0:
			return nil
		case syscall.EINTR:
			continue
		default:
			return errno
		}
	}
}

func (r *ioUringRing) waitCQE(want uint64) (int32, error) {
	for {
		head := atomic.LoadUint32(r.cqKHead)
		tail := atomic.LoadUint32(r.cqKTail)
		for head != tail {
			cqe := &r.cqes[head&r.cqMask]
			ud := cqe.UserData
			res := cqe.Res
			head++
			atomic.StoreUint32(r.cqKHead, head)
			if ud != want {
				continue
			}
			if res < 0 {
				return 0, syscall.Errno(-res)
			}
			return res, nil
		}
		if err := r.enter(0, 1); err != nil {
			return 0, err
		}
	}
}

// sendHeaderOnly submits a single WRITE SQE and waits for its CQE.
func (r *ioUringRing) sendHeaderOnly(sockFd int, header []byte) (int, error) {
	if len(header) == 0 {
		return 0, nil
	}
	sqe, err := r.prepSqe()
	if err != nil {
		return 0, err
	}
	sqe.setOpcode(ioringOpWrite)
	sqe.setFd(int32(sockFd))
	sqe.setAddr(uint64(uintptr(unsafe.Pointer(&header[0]))))
	sqe.setLen(uint32(len(header)))
	sqe.setUserData(sqeUserDataHdr)
	r.sqCommit()
	runtime.KeepAlive(header)
	if err := r.enter(1, 1); err != nil {
		return 0, err
	}
	n, err := r.waitCQE(sqeUserDataHdr)
	return int(n), err
}

// sendHeaderAndFile submits WRITE(header) linked to SPLICE(file→socket) and
// waits for both CQEs.  Returns total bytes shipped (header + body).
// SPLICE is kept for experiments; the production path uses sendHeaderOnly
// + sendFileViaRaw until splice-to-socket performance is validated on metal.
func (r *ioUringRing) sendHeaderAndFile(sockFd, fileFd int, fileOff, bodyLen int64, header []byte) (int64, error) {
	if len(header) == 0 && bodyLen == 0 {
		return 0, nil
	}
	if len(header) > 0 && bodyLen > 0 && r.sqSpace() < 2 {
		return 0, errors.New("io_uring SQ needs 2 slots")
	}

	var hdrLen int
	if len(header) > 0 {
		sqe, err := r.prepSqe()
		if err != nil {
			return 0, err
		}
		sqe.setOpcode(ioringOpWrite)
		sqe.setFlags(iosqeIoLink)
		sqe.setFd(int32(sockFd))
		sqe.setAddr(uint64(uintptr(unsafe.Pointer(&header[0]))))
		sqe.setLen(uint32(len(header)))
		sqe.setUserData(sqeUserDataHdr)
		r.sqCommit()
		hdrLen = len(header)
	}

	if bodyLen > 0 {
		sqe, err := r.prepSqe()
		if err != nil {
			return 0, err
		}
		sqe.setOpcode(ioringOpSplice)
		sqe.setFd(int32(sockFd))
		sqe.setOff(-1) // append to socket
		sqe.setAddr(uint64(fileOff))
		sqe.setLen(uint32(bodyLen))
		sqe.setSpliceFdIn(int32(fileFd))
		sqe.setUserData(sqeUserDataBody)
		r.sqCommit()
	}

	toSubmit := uint32(0)
	if hdrLen > 0 {
		toSubmit++
	}
	if bodyLen > 0 {
		toSubmit++
	}
	minComplete := toSubmit

	// Ensure header memory is not moved before enter returns.
	runtime.KeepAlive(header)

	if err := r.enter(toSubmit, minComplete); err != nil {
		return 0, err
	}

	var total int64
	if hdrLen > 0 {
		n, err := r.waitCQE(sqeUserDataHdr)
		if err != nil {
			return total, err
		}
		if int(n) != hdrLen {
			return int64(n), ioErrShortWrite
		}
		total += int64(n)
	}
	if bodyLen > 0 {
		n, err := r.waitCQE(sqeUserDataBody)
		if err != nil {
			return total, err
		}
		total += int64(n)
	}
	return total, nil
}

var ioErrShortWrite = errors.New("coreserver: io_uring short write")

// sendSplice ships bodyLen bytes from fileFd@fileOff into sockFd.
func (r *ioUringRing) sendSplice(sockFd, fileFd int, fileOff, bodyLen int64) (int64, error) {
	if bodyLen <= 0 {
		return 0, nil
	}
	if bodyLen > int64(^uint32(0)) {
		bodyLen = int64(^uint32(0))
	}
	sqe, err := r.prepSqe()
	if err != nil {
		return 0, err
	}
	sqe.setOpcode(ioringOpSplice)
	sqe.setFd(int32(sockFd))
	sqe.setOff(-1)
	sqe.setAddr(uint64(fileOff))
	sqe.setLen(uint32(bodyLen))
	sqe.setSpliceFdIn(int32(fileFd))
	sqe.setUserData(sqeUserDataBody)
	r.sqCommit()
	if err := r.enter(1, 1); err != nil {
		return 0, err
	}
	n, err := r.waitCQE(sqeUserDataBody)
	return int64(n), err
}

// ioUringPool holds one ring per GOMAXPROCS slot; each ring has its own
// mutex so SINGLE_ISSUER holds per ring while requests stripe across rings.
type ioUringPool struct {
	rings   []*ioUringRing
	mu      []sync.Mutex
	strip   atomic.Uint32
	once    sync.Once
	initErr error
}

func (p *ioUringPool) init(flags uint32) error {
	p.once.Do(func() {
		n := runtime.GOMAXPROCS(0)
		if n < 1 {
			n = 1
		}
		p.rings = make([]*ioUringRing, n)
		p.mu = make([]sync.Mutex, n)
		for i := 0; i < n; i++ {
			ring, err := newIoUringRing(256, flags)
			if err != nil {
				p.initErr = err
				for j := 0; j < i; j++ {
					p.rings[j].close()
				}
				p.rings = nil
				p.mu = nil
				return
			}
			p.rings[i] = ring
		}
	})
	return p.initErr
}

func (p *ioUringPool) close() {
	for _, r := range p.rings {
		if r != nil {
			r.close()
		}
	}
}

func (p *ioUringPool) withRing(fn func(*ioUringRing) (int64, error)) (int64, error) {
	if len(p.rings) == 0 {
		return 0, errIoUringUnavailable
	}
	i := int(p.strip.Add(1) % uint32(len(p.rings)))
	p.mu[i].Lock()
	defer p.mu[i].Unlock()
	return fn(p.rings[i])
}
