//go:build linux && (amd64 || arm64)

package alsa

import (
	"syscall"
	"testing"
	"unsafe"
)

func TestALSAABILayouts(t *testing.T) {
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"snd_mask", unsafe.Sizeof(sndMask{}), 32},
		{"snd_interval", unsafe.Sizeof(sndInterval{}), 12},
		{"snd_pcm_hw_params", unsafe.Sizeof(hwParams{}), 608},
		{"snd_pcm_sw_params", unsafe.Sizeof(swParams{}), 136},
		{"snd_xferi", unsafe.Sizeof(xferi{}), 24},
		{"snd_ctl_card_info", unsafe.Sizeof(sndCtlCardInfo{}), 376},
		{"snd_pcm_info", unsafe.Sizeof(sndPCMInfo{}), 288},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("size = %d, want %d", test.got, test.want)
			}
		})
	}
	offsets := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"hw_params.masks", unsafe.Offsetof(hwParams{}.masks), 4},
		{"hw_params.intervals", unsafe.Offsetof(hwParams{}.intervals), 260},
		{"hw_params.rmask", unsafe.Offsetof(hwParams{}.rmask), 512},
		{"hw_params.fifo_size", unsafe.Offsetof(hwParams{}.fifoSize), 536},
		{"hw_params.reserved", unsafe.Offsetof(hwParams{}.reserved), 544},
		{"sw_params.avail_min", unsafe.Offsetof(swParams{}.availMin), 16},
		{"sw_params.boundary", unsafe.Offsetof(swParams{}.boundary), 64},
		{"sw_params.proto", unsafe.Offsetof(swParams{}.proto), 72},
		{"sw_params.reserved", unsafe.Offsetof(swParams{}.reserved), 80},
		{"xferi.buf", unsafe.Offsetof(xferi{}.buf), 8},
		{"xferi.frames", unsafe.Offsetof(xferi{}.frames), 16},
		{"pcm_info.sync", unsafe.Offsetof(sndPCMInfo{}.sync), 208},
		{"pcm_info.reserved", unsafe.Offsetof(sndPCMInfo{}.reserved), 224},
	}
	for _, test := range offsets {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("offset = %d, want %d", test.got, test.want)
			}
		})
	}
}

func TestALSAIOCTLRequestCodes(t *testing.T) {
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"SNDRV_CTL_IOCTL_CARD_INFO", ioctlCtlCardInfo, 0x81785501},
		{"SNDRV_CTL_IOCTL_PCM_NEXT_DEVICE", ioctlCtlPCMNextDevice, 0x80045530},
		{"SNDRV_CTL_IOCTL_PCM_INFO", ioctlCtlPCMInfo, 0xc1205531},
		{"SNDRV_PCM_IOCTL_PVERSION", ioctlPCMPVersion, 0x80044100},
		{"SNDRV_PCM_IOCTL_TTSTAMP", ioctlPCMTTStamp, 0x40044103},
		{"SNDRV_PCM_IOCTL_USER_PVERSION", ioctlPCMUserPVersion, 0x40044104},
		{"SNDRV_PCM_IOCTL_HW_REFINE", ioctlPCMHWRefine, 0xc2604110},
		{"SNDRV_PCM_IOCTL_HW_PARAMS", ioctlPCMHWParams, 0xc2604111},
		{"SNDRV_PCM_IOCTL_SW_PARAMS", ioctlPCMSWParams, 0xc0884113},
		{"SNDRV_PCM_IOCTL_DELAY", ioctlPCMDelay, 0x80084121},
		{"SNDRV_PCM_IOCTL_PREPARE", ioctlPCMPrepare, 0x00004140},
		{"SNDRV_PCM_IOCTL_START", ioctlPCMStart, 0x00004142},
		{"SNDRV_PCM_IOCTL_DROP", ioctlPCMDrop, 0x00004143},
		{"SNDRV_PCM_IOCTL_RESUME", ioctlPCMResume, 0x00004147},
		{"SNDRV_PCM_IOCTL_WRITEI_FRAMES", ioctlPCMWriteI, 0x40184150},
		{"SNDRV_PCM_IOCTL_READI_FRAMES", ioctlPCMReadI, 0x80184151},
		{"SNDRV_PCM_IOCTL_LINK", ioctlPCMLink, 0x40044160},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("request = %#08x, want %#08x", test.got, test.want)
			}
		})
	}
}

func TestIOCTLEncoding(t *testing.T) {
	if got := ioc(iocRead|iocWrite, 'A', 0x10, 608); got != 0xc2604110 {
		t.Fatalf("generic encoding = %#08x, want 0xc2604110", got)
	}
}

func TestIOCTLReturnsRawErrno(t *testing.T) {
	var version int32
	err := ioctl(-1, ioctlPCMPVersion, unsafe.Pointer(&version))
	errno, ok := err.(syscall.Errno)
	if !ok || errno != syscall.EBADF {
		t.Fatalf("ioctl error = %#v, want raw syscall.EBADF", err)
	}
}

func TestHWParamsHelpers(t *testing.T) {
	p := hwParamsAnything()
	if p.rmask != (1<<20)-1 {
		t.Fatalf("rmask = %#x, want all 20 parameter bits", p.rmask)
	}
	for i, mask := range p.masks {
		for word, bits := range mask.bits {
			if bits != ^uint32(0) {
				t.Fatalf("mask %d word %d = %#x, want all bits", i, word, bits)
			}
		}
	}
	for i, interval := range p.intervals {
		if interval.min != 0 || interval.max != ^uint32(0) || interval.flags != 0 {
			t.Fatalf("interval %d = %#v, want [0, MaxUint32]", i+8, interval)
		}
	}

	if !hwSetMask(&p, hwParamFormat, 32) || !hwMaskHas(p, hwParamFormat, 32) || hwMaskHas(p, hwParamFormat, 31) {
		t.Fatal("mask constraint did not select only format bit 32")
	}
	if codes := supportedPCMFormatCodes(p); len(codes) != 1 || codes[0] != 32 {
		t.Fatalf("supported format codes = %v, want [32]", codes)
	}
	if !hwSetInterval(&p, hwParamPeriodSize, 128, 256, true) {
		t.Fatal("valid period interval was rejected")
	}
	min, max, flags, ok := hwIntervalRange(p, hwParamPeriodSize)
	if !ok || min != 128 || max != 256 || flags&intervalInteger == 0 {
		t.Fatalf("period range = [%d,%d], flags %#x, ok=%t", min, max, flags, ok)
	}
	if _, _, _, ok := hwIntervalRange(p, 7); ok {
		t.Fatal("reserved parameter index unexpectedly mapped to an interval")
	}
	if minimum, ok := intervalMinimum(sndInterval{min: 128, max: 256, flags: intervalOpenMin}); !ok || minimum != 129 {
		t.Fatalf("open minimum = %d, ok=%t, want 129", minimum, ok)
	}
	if _, ok := intervalMinimum(sndInterval{min: 128, max: 128, flags: intervalOpenMax}); ok {
		t.Fatal("empty open-ended interval unexpectedly had a minimum")
	}
}

func TestSoftwareParameterDefaults(t *testing.T) {
	p := makeSWParams(256, 1024)
	if p.tstampMode != 1 || p.tstampType != 1 || p.periodStep != 1 || p.availMin != 256 || p.xferAlign != 1 {
		t.Fatalf("timestamp/wakeup settings = %#v", p)
	}
	if p.startThreshold != 1024 || p.stopThreshold != 1024 || p.silenceThreshold != 0 || p.silenceSize != 0 || p.boundary != 0 {
		t.Fatalf("buffer thresholds = %#v", p)
	}
}
