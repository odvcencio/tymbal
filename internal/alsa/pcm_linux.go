//go:build linux && (amd64 || arm64)

package alsa

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
)

const (
	pcmPollIn           = int16(0x001)
	pcmPollOut          = int16(0x004)
	pcmPollErr          = int16(0x008)
	pcmPollHup          = int16(0x010)
	pcmPollNval         = int16(0x020)
	pcmArenaRO          = syscall.PROT_READ | syscall.PROT_WRITE
	pcmArenaMap         = syscall.MAP_PRIVATE | syscall.MAP_ANON
	pcmClockMonotonic   = 1
	pcmKernelSigsetSize = 8
	pcmNoProgressLimit  = 8
)

var (
	errPCMState    = errors.New("alsa: invalid PCM stream state")
	errPCMTransfer = errors.New("alsa: invalid PCM transfer result")
	errPCMNoDevice = errors.New("alsa: no PCM direction configured")
)

// pcmPollFD matches Linux's struct pollfd on the supported 64-bit targets.
type pcmPollFD struct {
	fd      int32
	events  int16
	revents int16
}

type pcmTimespec struct {
	sec  int64
	nsec int64
}

type pcmEndpoint struct {
	fd         int
	params     pcmParams
	arena      []byte
	data       []byte
	xfer       *xferi
	frameBytes int
	periodByte int
	writeReq   uintptr
	readReq    uintptr
}

type pcmOps struct {
	ioctl func(fd int, request uintptr, arg unsafe.Pointer) error
	ppoll func(fds *[3]pcmPollFD, count int) (int, syscall.Errno)
}

type pcmStream struct {
	params driver.Params
	out    pcmEndpoint
	in     pcmEndpoint
	hasOut bool
	hasIn  bool
	linked bool
	owned  bool

	wakeRead    int
	wakeWrite   int
	wakeMu      sync.Mutex
	closed      atomic.Bool
	interrupted atomic.Bool
	wakeByte    [1]byte
	clockTS     pcmTimespec
	outDelay    int64
	inDelay     int64
	linkFD      int32
	tstampMode  int32
	readByte    [1]byte

	mainPoll  [3]pcmPollFD
	mainN     int
	outIndex  int
	inIndex   int
	wakeIndex int
	xferPoll  [3]pcmPollFD

	ops pcmOps

	started    bool
	stopped    bool
	dropouts   uint64
	recoverErr syscall.Errno
	outReady   bool
	inReady    bool
}

// openPCMStream creates the wait primitive and the PCM transfer arenas. On
// success it takes ownership of outFD and inFD. On failure the caller retains
// both file descriptors and must close them.
func openPCMStream(outFD, inFD int, outP, inP pcmParams) (*pcmStream, error) {
	return newPCMStream(outFD, inFD, outP, inP, pcmOps{ioctl: ioctl, ppoll: pcmPPoll}, true)
}

func newPCMStream(outFD, inFD int, outP, inP pcmParams, ops pcmOps, owned bool) (*pcmStream, error) {
	if outFD < 0 && inFD < 0 {
		return nil, errPCMNoDevice
	}
	if ops.ioctl == nil || ops.ppoll == nil {
		return nil, fmt.Errorf("alsa: missing PCM system call implementation")
	}

	params := driver.Params{}
	if outFD >= 0 {
		if err := validatePCMParams(outP); err != nil {
			return nil, fmt.Errorf("alsa: output: %w", err)
		}
		params.SampleRate = outP.rate
		params.Period = outP.period
		params.Periods = outP.periods
		params.OutChannels = outP.channels
		params.OutFormat = outP.format
		params.LatencyOut = pcmLatency(outP)
	}
	if inFD >= 0 {
		if err := validatePCMParams(inP); err != nil {
			return nil, fmt.Errorf("alsa: input: %w", err)
		}
		if outFD >= 0 && (outP.rate != inP.rate || outP.period != inP.period || outP.periods != inP.periods) {
			return nil, fmt.Errorf("alsa: duplex PCM parameters do not share a clock and period")
		}
		if outFD < 0 {
			params.SampleRate = inP.rate
			params.Period = inP.period
			params.Periods = inP.periods
		}
		params.InChannels = inP.channels
		params.InFormat = inP.format
		params.LatencyIn = pcmLatency(inP)
	}

	s := &pcmStream{
		params:     params,
		wakeRead:   -1,
		wakeWrite:  -1,
		outIndex:   -1,
		inIndex:    -1,
		wakeIndex:  -1,
		ops:        ops,
		owned:      owned,
		tstampMode: 1, // SNDRV_PCM_TSTAMP_TYPE_MONOTONIC
	}
	if outFD >= 0 {
		e, err := newPCMEndpoint(outFD, outP, ioctlPCMWriteI)
		if err != nil {
			return nil, fmt.Errorf("alsa: output arena: %w", err)
		}
		s.out = e
		s.hasOut = true
	}
	if inFD >= 0 {
		e, err := newPCMEndpoint(inFD, inP, ioctlPCMReadI)
		if err != nil {
			if s.hasOut {
				_ = syscall.Munmap(s.out.arena)
			}
			return nil, fmt.Errorf("alsa: input arena: %w", err)
		}
		s.in = e
		s.hasIn = true
	}
	var pipe [2]int
	if err := syscall.Pipe2(pipe[:], syscall.O_CLOEXEC|syscall.O_NONBLOCK); err != nil {
		if s.hasOut {
			_ = syscall.Munmap(s.out.arena)
		}
		if s.hasIn {
			_ = syscall.Munmap(s.in.arena)
		}
		return nil, fmt.Errorf("alsa: create interrupt pipe: %w", err)
	}
	s.wakeRead, s.wakeWrite = pipe[0], pipe[1]
	s.buildPollSet()
	return s, nil
}

func validatePCMParams(p pcmParams) error {
	if p.rate <= 0 || p.channels <= 0 || p.period <= 0 || p.periods <= 0 || format.BytesPerSample(p.format) == 0 {
		return fmt.Errorf("invalid negotiated PCM parameters")
	}
	period := uint64(p.period)
	periods := uint64(p.periods)
	if periods > math.MaxUint64/period {
		return fmt.Errorf("negotiated buffer geometry overflows")
	}
	expectedFrames := period * periods
	if p.bufferFrames != 0 && (p.bufferFrames != expectedFrames || p.bufferFrames%period != 0) {
		return fmt.Errorf("buffer contains %d frames, want %d whole periods", p.bufferFrames, expectedFrames)
	}
	return nil
}

func pcmLatency(p pcmParams) time.Duration {
	frames := p.bufferFrames
	if frames == 0 {
		frames = uint64(p.period) * uint64(p.periods)
	}
	return time.Duration(frames * uint64(time.Second) / uint64(p.rate))
}

func newPCMEndpoint(fd int, p pcmParams, req uintptr) (pcmEndpoint, error) {
	bps := format.BytesPerSample(p.format)
	maxInt := int(^uint(0) >> 1)
	if p.channels > maxInt/bps || p.period > maxInt/(p.channels*bps) {
		return pcmEndpoint{}, fmt.Errorf("period buffer size overflows int")
	}
	frameBytes := p.channels * bps
	periodBytes := p.period * frameBytes
	xferSize := int(unsafe.Sizeof(xferi{}))
	dataOffset := (xferSize + 7) &^ 7
	if periodBytes > maxInt-dataOffset {
		return pcmEndpoint{}, fmt.Errorf("period arena size overflows int")
	}
	arena, err := syscall.Mmap(-1, 0, dataOffset+periodBytes, pcmArenaRO, pcmArenaMap)
	if err != nil {
		return pcmEndpoint{}, err
	}
	x := (*xferi)(unsafe.Pointer(&arena[0]))
	data := arena[dataOffset : dataOffset+periodBytes]
	*x = xferi{buf: uintptr(unsafe.Pointer(&data[0])), frames: uint64(p.period)}
	return pcmEndpoint{
		fd:         fd,
		params:     p,
		arena:      arena,
		data:       data,
		xfer:       x,
		frameBytes: frameBytes,
		periodByte: periodBytes,
		writeReq:   ioctlPCMWriteI,
		readReq:    req,
	}, nil
}

func (s *pcmStream) buildPollSet() {
	s.mainN = 0
	s.outIndex, s.inIndex, s.wakeIndex = -1, -1, -1
	if s.hasOut {
		s.outIndex = s.mainN
		s.mainPoll[s.mainN] = pcmPollFD{fd: int32(s.out.fd), events: pcmPollOut}
		s.mainN++
	}
	if s.hasIn {
		s.inIndex = s.mainN
		s.mainPoll[s.mainN] = pcmPollFD{fd: int32(s.in.fd), events: pcmPollIn}
		s.mainN++
	}
	s.wakeIndex = s.mainN
	s.mainPoll[s.mainN] = pcmPollFD{fd: int32(s.wakeRead), events: pcmPollIn}
	s.mainN++
}

//tymbal:rt
func (s *pcmStream) Params() driver.Params { return s.params }

//tymbal:rt
func (s *pcmStream) Start() error {
	if s.closed.Load() || s.started || s.stopped || s.interrupted.Load() {
		return errPCMState
	}
	for _, ep := range s.endpoints() {
		if ep == nil {
			continue
		}
		if err := s.ops.ioctl(ep.fd, ioctlPCMTTStamp, unsafe.Pointer(&s.tstampMode)); err != nil {
			return s.mapOperationError(err)
		}
		if err := s.ops.ioctl(ep.fd, ioctlPCMPrepare, nil); err != nil {
			return s.mapOperationError(err)
		}
	}
	if s.hasOut && s.hasIn {
		s.linkFD = int32(s.in.fd)
		if err := s.ops.ioctl(s.out.fd, ioctlPCMLink, unsafe.Pointer(&s.linkFD)); err != nil {
			return s.mapOperationError(err)
		}
		s.linked = true
	}
	s.started = true
	if s.hasOut {
		clear(s.out.data)
		for i := 0; i < s.out.params.periods; i++ {
			if err := s.transferFull(&s.out, s.out.writeReq, false); err != nil {
				return s.noteXrun(err)
			}
		}
	}
	if s.hasIn && !s.hasOut {
		if err := s.ops.ioctl(s.in.fd, ioctlPCMStart, nil); err != nil {
			return s.mapOperationError(err)
		}
	}
	return nil
}

//tymbal:rt
func (s *pcmStream) Wait() error {
	if s.closed.Load() || !s.started || s.stopped {
		return errPCMState
	}
	if s.interrupted.Load() {
		return driver.ErrInterrupted
	}
	if s.hasOut {
		s.outReady = false
		s.mainPoll[s.outIndex] = pcmPollFD{fd: int32(s.out.fd), events: pcmPollOut}
	}
	if s.hasIn {
		s.inReady = false
		s.mainPoll[s.inIndex] = pcmPollFD{fd: int32(s.in.fd), events: pcmPollIn}
	}
	for {
		for i := 0; i < s.mainN; i++ {
			s.mainPoll[i].revents = 0
		}
		_, errno := s.ops.ppoll(&s.mainPoll, s.mainN)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return s.mapOperationError(errno)
		}
		if s.mainPoll[s.wakeIndex].revents != 0 {
			s.drainWake()
			if s.interrupted.Load() {
				return driver.ErrInterrupted
			}
		}
		if s.interrupted.Load() {
			return driver.ErrInterrupted
		}
		if s.pollLost() {
			return driver.ErrLost
		}
		if s.hasOut && !s.outReady && pollReadyFor(s.mainPoll[s.outIndex].revents, pcmPollOut) {
			s.outReady = true
			s.mainPoll[s.outIndex].fd = -1
		}
		if s.hasIn && !s.inReady && pollReadyFor(s.mainPoll[s.inIndex].revents, pcmPollIn) {
			s.inReady = true
			s.mainPoll[s.inIndex].fd = -1
		}
		if s.hasOut && !s.outReady || s.hasIn && !s.inReady {
			continue
		}
		if s.hasIn {
			if err := s.transferFull(&s.in, s.in.readReq, true); err != nil {
				return s.noteXrun(err)
			}
		}
		return nil
	}
}

func (s *pcmStream) Interrupt() {
	if s == nil || s.interrupted.Swap(true) {
		return
	}
	s.wakeMu.Lock()
	if !s.closed.Load() && s.wakeWrite >= 0 {
		_, _, _ = syscall.RawSyscall(syscall.SYS_WRITE, uintptr(s.wakeWrite), uintptr(unsafe.Pointer(&s.wakeByte[0])), 1)
	}
	s.wakeMu.Unlock()
}

//tymbal:rt
func (s *pcmStream) Buffers() (in, out []byte) {
	if s.hasIn {
		in = s.in.data
	}
	if s.hasOut {
		out = s.out.data
	}
	return in, out
}

//tymbal:rt
func (s *pcmStream) Commit() error {
	if s.closed.Load() || !s.started || s.stopped {
		return errPCMState
	}
	if !s.hasOut {
		return nil
	}
	return s.noteXrun(s.transferFull(&s.out, s.out.writeReq, false))
}

//tymbal:rt
func (s *pcmStream) Clock() (outNano, inNano int64) {
	now, ok := pcmNow(&s.clockTS)
	if !ok {
		return 0, 0
	}
	if s.hasOut {
		s.outDelay = 0
		if s.ops.ioctl(s.out.fd, ioctlPCMDelay, unsafe.Pointer(&s.outDelay)) == nil {
			outNano = now + s.outDelay*int64(time.Second)/int64(s.out.params.rate)
		}
	}
	if s.hasIn {
		s.inDelay = 0
		if s.ops.ioctl(s.in.fd, ioctlPCMDelay, unsafe.Pointer(&s.inDelay)) == nil {
			inNano = now - s.inDelay*int64(time.Second)/int64(s.in.params.rate)
		}
	}
	return outNano, inNano
}

//tymbal:rt
func (s *pcmStream) Deadlines() (wakeNano, commitNano int64) { return 0, 0 }

//tymbal:rt
func (s *pcmStream) Dropouts() uint64 { return s.dropouts }

//tymbal:rt
func (s *pcmStream) Recover() error {
	if s.closed.Load() || !s.started || s.stopped {
		return errPCMState
	}
	resumeOK := s.recoverErr == syscall.ESTRPIPE
	if resumeOK {
		for _, ep := range s.endpoints() {
			if ep == nil {
				continue
			}
			resumed := false
			for attempt := 0; attempt < 64; attempt++ {
				err := s.ops.ioctl(ep.fd, ioctlPCMResume, nil)
				if err == nil {
					resumed = true
					break
				}
				if errno, ok := pcmErrno(err); ok && errno == syscall.EAGAIN {
					continue
				}
				if mapped := s.mapOperationError(err); mapped == driver.ErrLost {
					return mapped
				}
				resumeOK = false
				break
			}
			if !resumed {
				resumeOK = false
			}
		}
	}
	if resumeOK {
		s.recoverErr = 0
		return nil
	}
	for _, ep := range s.endpoints() {
		if ep == nil {
			continue
		}
		if err := s.ops.ioctl(ep.fd, ioctlPCMPrepare, nil); err != nil {
			return s.mapOperationError(err)
		}
	}
	if s.hasOut {
		clear(s.out.data)
		for i := 0; i < s.out.params.periods; i++ {
			if err := s.transferFull(&s.out, s.out.writeReq, false); err != nil {
				return s.mapOperationError(err)
			}
		}
	}
	if s.hasIn && !s.hasOut {
		if err := s.ops.ioctl(s.in.fd, ioctlPCMStart, nil); err != nil {
			return s.mapOperationError(err)
		}
	}
	s.recoverErr = 0
	return nil
}

//tymbal:rt
func (s *pcmStream) Stop() error {
	if s.stopped {
		return nil
	}
	s.stopped = true
	if !s.started {
		return nil
	}
	var first error
	for _, ep := range s.endpoints() {
		if ep == nil {
			continue
		}
		if err := s.ops.ioctl(ep.fd, ioctlPCMDrop, nil); err != nil && first == nil {
			if !isBenignStopError(err) {
				first = s.mapOperationError(err)
			}
		}
	}
	return first
}

func (s *pcmStream) Close() error {
	if s == nil {
		return nil
	}
	s.Interrupt()
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.closed.Swap(true) {
		return nil
	}
	var first error
	if s.owned {
		if s.hasOut {
			if err := syscall.Close(s.out.fd); err != nil && first == nil {
				first = err
			}
		}
		if s.hasIn {
			if err := syscall.Close(s.in.fd); err != nil && first == nil {
				first = err
			}
		}
	}
	if s.hasOut {
		if err := syscall.Munmap(s.out.arena); err != nil && first == nil {
			first = err
		}
	}
	if s.hasIn {
		if err := syscall.Munmap(s.in.arena); err != nil && first == nil {
			first = err
		}
	}
	if s.wakeRead >= 0 {
		if err := syscall.Close(s.wakeRead); err != nil && first == nil {
			first = err
		}
		s.wakeRead = -1
	}
	if s.wakeWrite >= 0 {
		if err := syscall.Close(s.wakeWrite); err != nil && first == nil {
			first = err
		}
		s.wakeWrite = -1
	}
	return first
}

//tymbal:rt
func (s *pcmStream) endpoints() [2]*pcmEndpoint {
	var eps [2]*pcmEndpoint
	if s.hasOut {
		eps[0] = &s.out
	}
	if s.hasIn {
		if s.hasOut {
			eps[1] = &s.in
		} else {
			eps[0] = &s.in
		}
	}
	return eps
}

//tymbal:rt
func (s *pcmStream) transferFull(ep *pcmEndpoint, request uintptr, interruptible bool) error {
	framesDone := 0
	noProgress := 0
	for framesDone < ep.params.period {
		remaining := ep.params.period - framesDone
		offset := framesDone * ep.frameBytes
		ep.xfer.result = 0
		ep.xfer.buf = uintptr(unsafe.Pointer(&ep.data[offset]))
		ep.xfer.frames = uint64(remaining)
		err := s.ops.ioctl(ep.fd, request, unsafe.Pointer(ep.xfer))
		if err != nil {
			if errno, ok := pcmErrno(err); ok {
				switch errno {
				case syscall.EINTR:
					continue
				case syscall.EAGAIN:
					noProgress++
					if noProgress >= pcmNoProgressLimit {
						return syscall.EIO
					}
					if waitErr := s.waitReady(ep.fd, pcmEvents(request), interruptible); waitErr != nil {
						return waitErr
					}
					continue
				}
			}
			return s.mapOperationError(err)
		}
		transferred := ep.xfer.result
		if transferred < 0 {
			errno := syscall.Errno(-transferred)
			switch errno {
			case syscall.EINTR:
				continue
			case syscall.EAGAIN:
				noProgress++
				if noProgress >= pcmNoProgressLimit {
					return syscall.EIO
				}
				if waitErr := s.waitReady(ep.fd, pcmEvents(request), interruptible); waitErr != nil {
					return waitErr
				}
				continue
			}
			return s.mapOperationError(errno)
		}
		if transferred == 0 {
			noProgress++
			if noProgress >= pcmNoProgressLimit {
				return syscall.EIO
			}
			if waitErr := s.waitReady(ep.fd, pcmEvents(request), interruptible); waitErr != nil {
				return waitErr
			}
			continue
		}
		if uint64(transferred) > uint64(remaining) {
			return errPCMTransfer
		}
		noProgress = 0
		framesDone += int(transferred)
	}
	return nil
}

//tymbal:rt
func (s *pcmStream) waitReady(fd int, events int16, interruptible bool) error {
	s.xferPoll[0] = pcmPollFD{fd: int32(fd), events: events}
	s.xferPoll[1] = pcmPollFD{fd: int32(s.wakeRead), events: pcmPollIn}
	for {
		s.xferPoll[0].revents = 0
		s.xferPoll[1].revents = 0
		_, errno := s.ops.ppoll(&s.xferPoll, 2)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return s.mapOperationError(errno)
		}
		if s.xferPoll[1].revents != 0 {
			s.drainWake()
			if interruptible && s.interrupted.Load() {
				return driver.ErrInterrupted
			}
		}
		if interruptible && s.interrupted.Load() {
			return driver.ErrInterrupted
		}
		if s.xferPoll[0].revents&pcmPollNval != 0 {
			return driver.ErrLost
		}
		if pollReadyFor(s.xferPoll[0].revents, events) {
			return nil
		}
	}
}

//tymbal:rt
func (s *pcmStream) drainWake() {
	_, _, _ = syscall.RawSyscall(syscall.SYS_READ, uintptr(s.wakeRead), uintptr(unsafe.Pointer(&s.readByte[0])), 1)
}

//tymbal:rt
func (s *pcmStream) pollLost() bool {
	if s.hasOut && s.mainPoll[s.outIndex].revents&pcmPollNval != 0 {
		return true
	}
	return s.hasIn && s.mainPoll[s.inIndex].revents&pcmPollNval != 0
}

//tymbal:rt
func (s *pcmStream) noteXrun(err error) error {
	if err == driver.ErrXrun {
		saturatingIncrement(&s.dropouts)
		if s.recoverErr == 0 {
			s.recoverErr = syscall.EPIPE
		}
		return err
	}
	if errno, ok := pcmErrno(err); ok && (errno == syscall.EPIPE || errno == syscall.ESTRPIPE) {
		saturatingIncrement(&s.dropouts)
		s.recoverErr = errno
		return driver.ErrXrun
	}
	return err
}

//tymbal:rt
func (s *pcmStream) mapOperationError(err error) error {
	if errno, ok := pcmErrno(err); ok {
		switch errno {
		case syscall.EPIPE, syscall.ESTRPIPE:
			s.recoverErr = errno
			return driver.ErrXrun
		case syscall.ENODEV, syscall.EBADFD:
			return driver.ErrLost
		default:
			return errno
		}
	}
	return err
}

//tymbal:rt
func pcmErrno(err error) (syscall.Errno, bool) {
	if err == nil {
		return 0, false
	}
	errno, ok := err.(syscall.Errno)
	return errno, ok
}

//tymbal:rt
func pollReadyFor(revents, events int16) bool {
	return revents&(events|pcmPollErr|pcmPollHup) != 0
}

//tymbal:rt
func pcmEvents(request uintptr) int16 {
	if request == ioctlPCMReadI {
		return pcmPollIn
	}
	return pcmPollOut
}

//tymbal:rt
func saturatingIncrement(v *uint64) {
	if *v != math.MaxUint64 {
		*v++
	}
}

//tymbal:rt
func pcmPPoll(fds *[3]pcmPollFD, count int) (int, syscall.Errno) {
	r, _, errno := syscall.RawSyscall6(syscall.SYS_PPOLL,
		uintptr(unsafe.Pointer(&fds[0])), uintptr(count), 0, 0, pcmKernelSigsetSize, 0)
	if errno != 0 {
		return int(r), errno
	}
	return int(r), 0
}

//tymbal:rt
func pcmNow(ts *pcmTimespec) (int64, bool) {
	_, _, errno := syscall.RawSyscall(syscall.SYS_CLOCK_GETTIME,
		pcmClockMonotonic, uintptr(unsafe.Pointer(ts)), 0)
	if errno != 0 {
		return 0, false
	}
	return ts.sec*int64(time.Second) + ts.nsec, true
}

//tymbal:rt
func isBenignStopError(err error) bool {
	if errno, ok := pcmErrno(err); ok {
		return errno == syscall.EBADFD || errno == syscall.EPIPE || errno == syscall.ENODEV
	}
	return false
}
