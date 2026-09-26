//go:build linux && (amd64 || arm64)

package alsa

import (
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
)

type pcmTestCall struct {
	fd      int
	request uintptr
	frames  uint64
	buf     uintptr
}

type fakePCM struct {
	mu sync.Mutex

	calls          []pcmTestCall
	writeShort     []uint64
	writeErrors    []syscall.Errno
	readShort      []uint64
	readErrors     []syscall.Errno
	resumeErrors   []syscall.Errno
	prepareErrors  []syscall.Errno
	starts         int
	links          int
	drops          int
	dataByte       byte
	delay          int64
	readFrameBytes int
	readPeriod     int
	readOffset     int
	readData       []byte
	wakeFD         int
}

func (f *fakePCM) ops() pcmOps {
	return pcmOps{ioctl: f.ioctl, ppoll: f.ppoll}
}

func (f *fakePCM) ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := pcmTestCall{fd: fd, request: request}
	switch request {
	case ioctlPCMWriteI, ioctlPCMReadI:
		x := (*xferi)(arg)
		call.frames, call.buf = x.frames, x.buf
		queue := &f.writeShort
		errors := &f.writeErrors
		if request == ioctlPCMReadI {
			queue = &f.readShort
			errors = &f.readErrors
		}
		if len(*errors) > 0 {
			errno := (*errors)[0]
			*errors = (*errors)[1:]
			x.result = -int64(errno)
			f.calls = append(f.calls, call)
			return nil
		}
		done := x.frames
		if len(*queue) > 0 {
			done = (*queue)[0]
			*queue = (*queue)[1:]
			if done > x.frames {
				done = x.frames
			}
		}
		if request == ioctlPCMReadI && done > 0 {
			readBytes := int(done) * f.readFrameBytes
			end := f.readOffset + readBytes
			if end > len(f.readData) {
				f.calls = append(f.calls, call)
				return syscall.EFAULT
			}
			for i := f.readOffset; i < end; i++ {
				f.readData[i] = f.dataByte
			}
			f.readOffset = end
			if f.readOffset == f.readPeriod*f.readFrameBytes {
				f.readOffset = 0
			}
			f.dataByte++
		}
		x.result = int64(done)
	case ioctlPCMTTStamp:
	case ioctlPCMPrepare:
		if len(f.prepareErrors) > 0 {
			errno := f.prepareErrors[0]
			f.prepareErrors = f.prepareErrors[1:]
			f.calls = append(f.calls, call)
			return errno
		}
	case ioctlPCMResume:
		if len(f.resumeErrors) > 0 {
			errno := f.resumeErrors[0]
			f.resumeErrors = f.resumeErrors[1:]
			f.calls = append(f.calls, call)
			return errno
		}
	case ioctlPCMStart:
		f.starts++
	case ioctlPCMLink:
		f.links++
	case ioctlPCMDrop:
		f.drops++
	case ioctlPCMDelay:
		*(*int64)(arg) = f.delay
	default:
		f.calls = append(f.calls, call)
		return syscall.EINVAL
	}
	f.calls = append(f.calls, call)
	return nil
}

func (f *fakePCM) ppoll(fds *[3]pcmPollFD, count int) (int, syscall.Errno) {
	nready := 0
	for i := 0; i < count; i++ {
		if fds[i].fd == int32(f.wakeFD) {
			fds[i].revents = 0
			continue
		}
		fds[i].revents = fds[i].events & (pcmPollIn | pcmPollOut)
		if fds[i].revents != 0 {
			nready++
		}
	}
	return nready, 0
}

func pcmTestParams(channels, period, periods int, f format.Format) pcmParams {
	bytes := format.BytesPerSample(f)
	frames := uint64(period * periods)
	return pcmParams{
		rate: 48000, channels: channels, period: period, periods: periods,
		format: f, bufferFrames: frames,
		bufferBytes: frames * uint64(channels*bytes),
	}
}

func openTestPCM(t *testing.T, outFD, inFD int, outP, inP pcmParams, fake *fakePCM) *pcmStream {
	t.Helper()
	if inFD >= 0 {
		fake.readFrameBytes = inP.channels * format.BytesPerSample(inP.format)
		fake.readPeriod = inP.period
	}
	s, err := newPCMStream(outFD, inFD, outP, inP, fake.ops(), false)
	if err != nil {
		t.Fatal(err)
	}
	if inFD >= 0 {
		fake.readData = s.in.data
	}
	fake.wakeFD = s.wakeRead
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestPCMPlaybackPrimesSilenceAndCommitsWholePeriod(t *testing.T) {
	fake := &fakePCM{}
	p := pcmTestParams(2, 4, 2, format.S16LE)
	s := openTestPCM(t, 101, -1, p, pcmParams{}, fake)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if got := s.Params(); got.SampleRate != 48000 || got.Period != 4 || got.Periods != 2 || got.OutChannels != 2 || got.OutFormat != format.S16LE {
		t.Fatalf("Params() = %+v", got)
	}
	if got := len(s.out.data); got != 4*2*2 {
		t.Fatalf("playback buffer has %d bytes, want %d", got, 16)
	}
	for i, b := range s.out.data {
		if b != 0 {
			t.Fatalf("prefill byte %d = %d, want silence", i, b)
		}
	}
	fake.writeShort = []uint64{2}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	_, out := s.Buffers()
	for i := range out {
		out[i] = 0x7f
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	writes := make([]pcmTestCall, 0, 4)
	for _, call := range fake.calls {
		if call.request == ioctlPCMWriteI {
			writes = append(writes, call)
		}
	}
	if len(writes) != 4 {
		t.Fatalf("saw %d writes, want two prefill and two partial-period writes", len(writes))
	}
	if writes[0].frames != 4 || writes[1].frames != 4 || writes[2].frames != 4 || writes[3].frames != 2 {
		t.Fatalf("write frame requests = [%d %d %d %d], want [4 4 4 2]", writes[0].frames, writes[1].frames, writes[2].frames, writes[3].frames)
	}
	if writes[3].buf-writes[2].buf != uintptr(2*2*2) {
		t.Fatalf("short-write pointer advanced %d bytes, want one 2-channel S16 frame pair", writes[3].buf-writes[2].buf)
	}
	if s.Dropouts() != 0 {
		t.Fatalf("Dropouts() = %d, want 0", s.Dropouts())
	}
	if wake, commit := s.Deadlines(); wake != 0 || commit != 0 || s.Params().HasDeadline {
		t.Fatalf("deadlines = (%d, %d), HasDeadline = %v; want unavailable", wake, commit, s.Params().HasDeadline)
	}
}

func TestPCMCaptureWaitFillsCompleteInputPeriod(t *testing.T) {
	fake := &fakePCM{readShort: []uint64{2}}
	p := pcmTestParams(1, 4, 2, format.S16LE)
	s := openTestPCM(t, -1, 102, pcmParams{}, p, fake)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if fake.starts != 1 {
		t.Fatalf("capture START calls = %d, want 1", fake.starts)
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	in, out := s.Buffers()
	if len(out) != 0 || len(in) != 4*2 {
		t.Fatalf("Buffers() lengths = (%d, %d), want (8, 0)", len(in), len(out))
	}
	for i, b := range in[:4] {
		if b != 0 {
			t.Fatalf("first short-read section byte %d = %d, want 0", i, b)
		}
	}
	for i, b := range in[4:] {
		if b != 1 {
			t.Fatalf("second short-read section byte %d = %d, want 1", i, b)
		}
	}
	var reads []pcmTestCall
	for _, call := range fake.calls {
		if call.request == ioctlPCMReadI {
			reads = append(reads, call)
		}
	}
	if len(reads) != 2 || reads[0].frames != 4 || reads[1].frames != 2 {
		t.Fatalf("read calls = %+v, want full read then 2-frame remainder", reads)
	}
	if reads[1].buf-reads[0].buf != uintptr(2*2) {
		t.Fatalf("short-read pointer advanced %d bytes, want 4", reads[1].buf-reads[0].buf)
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("capture-only Commit: %v", err)
	}
}

func TestPCMDuplexLinksBeforePrimingAndRunsBothSides(t *testing.T) {
	fake := &fakePCM{}
	outP := pcmTestParams(2, 4, 2, format.S16LE)
	inP := pcmTestParams(1, 4, 2, format.S32LE)
	s := openTestPCM(t, 103, 104, outP, inP, fake)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if fake.links != 1 || fake.starts != 0 {
		t.Fatalf("duplex LINK calls = %d, START calls = %d; want one link and threshold start", fake.links, fake.starts)
	}
	linkIndex, firstWrite := -1, -1
	for i, call := range fake.calls {
		if call.request == ioctlPCMLink && linkIndex < 0 {
			linkIndex = i
		}
		if call.request == ioctlPCMWriteI && firstWrite < 0 {
			firstWrite = i
		}
	}
	if linkIndex < 0 || firstWrite < 0 || linkIndex > firstWrite {
		t.Fatalf("duplex link/write order is %d/%d, want link before first prefill", linkIndex, firstWrite)
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	in, out := s.Buffers()
	if len(in) != 4*4 || len(out) != 4*2*2 {
		t.Fatalf("duplex buffer lengths = (%d, %d), want (16, 16)", len(in), len(out))
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := s.Params(); got.SampleRate != 48000 || got.Period != 4 || got.InFormat != format.S32LE || got.OutFormat != format.S16LE {
		t.Fatalf("duplex params = %+v", got)
	}
}

func TestPCMDuplexWaitMasksReadyEndpointWhileWaitingForPeer(t *testing.T) {
	fake := &fakePCM{}
	outP := pcmTestParams(1, 4, 2, format.S16LE)
	inP := pcmTestParams(1, 4, 2, format.S16LE)
	s := openTestPCM(t, 109, 110, outP, inP, fake)
	var pollCalls int
	var sawMaskedOutput bool
	s.ops.ppoll = func(fds *[3]pcmPollFD, count int) (int, syscall.Errno) {
		pollCalls++
		if pollCalls == 1 {
			fds[0].revents = pcmPollOut
			return 1, 0
		}
		sawMaskedOutput = fds[0].fd == -1
		fds[1].revents = pcmPollIn
		return 1, 0
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if pollCalls != 2 || !sawMaskedOutput {
		t.Fatalf("poll calls = %d, output masked on second call = %v; want 2 and true", pollCalls, sawMaskedOutput)
	}
}

func TestPCMRejectsBufferGeometryThatCannotMeetStartThreshold(t *testing.T) {
	p := pcmTestParams(2, 4, 2, format.S16LE)
	p.bufferFrames--
	if _, err := newPCMStream(111, -1, p, pcmParams{}, pcmOps{ioctl: noAllocPCMIOCTL, ppoll: noAllocPCMPoll}, false); err == nil {
		t.Fatal("newPCMStream accepted a buffer that does not equal period×periods")
	}
}

func TestPCMXrunCountsAndRecoversCapture(t *testing.T) {
	fake := &fakePCM{readErrors: []syscall.Errno{syscall.EPIPE}}
	p := pcmTestParams(1, 4, 2, format.S16LE)
	s := openTestPCM(t, -1, 105, pcmParams{}, p, fake)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); !errors.Is(err, driver.ErrXrun) {
		t.Fatalf("Wait() = %v, want ErrXrun", err)
	}
	if s.Dropouts() != 1 {
		t.Fatalf("Dropouts() = %d, want 1", s.Dropouts())
	}
	if err := s.Recover(); err != nil {
		t.Fatalf("Recover(): %v", err)
	}
	if fake.starts != 2 {
		t.Fatalf("capture START calls = %d, want restart after prepare", fake.starts)
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait after recovery: %v", err)
	}
}

func TestPCMPlaybackCommitXrunRecoversWithSilencePrefill(t *testing.T) {
	fake := &fakePCM{}
	p := pcmTestParams(1, 4, 2, format.S16LE)
	s := openTestPCM(t, 112, -1, p, pcmParams{}, fake)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	fake.writeErrors = []syscall.Errno{syscall.EPIPE}
	if err := s.Commit(); !errors.Is(err, driver.ErrXrun) {
		t.Fatalf("Commit() = %v, want ErrXrun", err)
	}
	if s.Dropouts() != 1 {
		t.Fatalf("Dropouts() = %d, want 1", s.Dropouts())
	}
	if err := s.Recover(); err != nil {
		t.Fatalf("Recover(): %v", err)
	}
	if got := countRequest(fake.calls, ioctlPCMWriteI); got != 5 {
		t.Fatalf("WRITEI calls = %d, want two initial, one failed, and two recovery silence periods", got)
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait after recovery: %v", err)
	}
}

func TestPCMResumeRetriesEAGAINThenRecovers(t *testing.T) {
	fake := &fakePCM{readErrors: []syscall.Errno{syscall.ESTRPIPE}, resumeErrors: []syscall.Errno{syscall.EAGAIN, syscall.EAGAIN}}
	p := pcmTestParams(1, 4, 2, format.S16LE)
	s := openTestPCM(t, -1, 106, pcmParams{}, p, fake)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); !errors.Is(err, driver.ErrXrun) {
		t.Fatalf("Wait() = %v, want ErrXrun", err)
	}
	if err := s.Recover(); err != nil {
		t.Fatalf("Recover(): %v", err)
	}
	if countRequest(fake.calls, ioctlPCMResume) != 3 {
		t.Fatalf("RESUME calls = %d, want 3", countRequest(fake.calls, ioctlPCMResume))
	}
	if countRequest(fake.calls, ioctlPCMPrepare) != 1 {
		t.Fatalf("PREPARE calls = %d, want only initial prepare after successful RESUME", countRequest(fake.calls, ioctlPCMPrepare))
	}
}

func TestPCMInterruptWakesBlockedWait(t *testing.T) {
	var pipe [2]int
	if err := syscall.Pipe2(pipe[:], syscall.O_CLOEXEC|syscall.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Close(pipe[0])
		_ = syscall.Close(pipe[1])
	})
	params := pcmTestParams(1, 4, 2, format.S16LE)
	entered := make(chan struct{})
	var once sync.Once
	blockingPoll := func(fds *[3]pcmPollFD, count int) (int, syscall.Errno) {
		once.Do(func() { close(entered) })
		return pcmPPoll(fds, count)
	}
	s, err := newPCMStream(-1, pipe[0], pcmParams{}, params, pcmOps{ioctl: noAllocPCMIOCTL, ppoll: blockingPoll}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- s.Wait() }()
	<-entered
	s.Interrupt()
	select {
	case err := <-waited:
		if !errors.Is(err, driver.ErrInterrupted) {
			t.Fatalf("Wait() = %v, want ErrInterrupted", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait remained blocked after Interrupt")
	}
}

func TestPCMStopCloseAreIdempotent(t *testing.T) {
	fake := &fakePCM{}
	p := pcmTestParams(1, 4, 2, format.S16LE)
	s := openTestPCM(t, 107, -1, p, pcmParams{}, fake)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if fake.drops != 1 {
		t.Fatalf("DROP calls = %d, want 1", fake.drops)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPCMSteadyStateMethodsDoNotAllocate(t *testing.T) {
	p := pcmTestParams(1, 4, 2, format.S16LE)
	s, err := newPCMStream(108, -1, p, pcmParams{}, pcmOps{ioctl: noAllocPCMIOCTL, ppoll: noAllocPCMPoll}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if err := s.Wait(); err != nil {
			panic(err)
		}
		_, out := s.Buffers()
		out[0] = byte(out[0] + 1)
		if err := s.Commit(); err != nil {
			panic(err)
		}
		_, _ = s.Clock()
		_ = s.Dropouts()
		_, _ = s.Deadlines()
	})
	if allocs != 0 {
		t.Fatalf("steady-state methods allocated %.2f objects per period", allocs)
	}
	interruptAllocs := testing.AllocsPerRun(100, func() {
		s.interrupted.Store(false)
		s.Interrupt()
	})
	if interruptAllocs != 0 {
		t.Fatalf("Interrupt allocated %.2f objects", interruptAllocs)
	}
}

func countRequest(calls []pcmTestCall, request uintptr) int {
	n := 0
	for _, call := range calls {
		if call.request == request {
			n++
		}
	}
	return n
}

func noAllocPCMIOCTL(_ int, request uintptr, arg unsafe.Pointer) error {
	switch request {
	case ioctlPCMWriteI, ioctlPCMReadI:
		x := (*xferi)(arg)
		x.result = int64(x.frames)
	case ioctlPCMDelay:
		*(*int64)(arg) = 0
	}
	return nil
}

func noAllocPCMPoll(fds *[3]pcmPollFD, count int) (int, syscall.Errno) {
	nready := 0
	for i := 0; i < count; i++ {
		if i == count-1 {
			fds[i].revents = 0
			continue
		}
		fds[i].revents = fds[i].events & (pcmPollIn | pcmPollOut)
		if fds[i].revents != 0 {
			nready++
		}
	}
	return nready, 0
}
