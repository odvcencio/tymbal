//go:build linux && (amd64 || arm64)

package alsa

// The layouts in this file mirror the 64-bit userspace ALSA ABI from
// <sound/asound.h>. snd_pcm_uframes_t and unsigned long are 64-bit on the
// supported targets (amd64 and arm64).

type sndMask struct {
	bits [8]uint32
}

type sndInterval struct {
	min, max uint32
	flags    uint32
}

const (
	intervalOpenMin uint32 = 1 << iota
	intervalOpenMax
	intervalInteger
	intervalEmpty
)

type hwParams struct {
	flags     uint32
	masks     [3]sndMask
	mres      [5]sndMask
	intervals [12]sndInterval
	ires      [9]sndInterval
	rmask     uint32
	cmask     uint32
	info      uint32
	msbits    uint32
	rateNum   uint32
	rateDen   uint32
	fifoSize  uint64
	reserved  [64]byte
}

type swParams struct {
	tstampMode       int32
	periodStep       uint32
	sleepMin         uint32
	_                uint32 // C ABI padding before snd_pcm_uframes_t.
	availMin         uint64
	xferAlign        uint64
	startThreshold   uint64
	stopThreshold    uint64
	silenceThreshold uint64
	silenceSize      uint64
	boundary         uint64
	proto            uint32
	tstampType       uint32
	reserved         [56]byte
}

type xferi struct {
	result int64
	buf    uintptr
	frames uint64
}

type sndCtlCardInfo struct {
	card       int32
	pad        int32
	id         [16]byte
	driver     [16]byte
	name       [32]byte
	longname   [80]byte
	reserved   [16]byte
	mixername  [80]byte
	components [128]byte
}

type sndPCMInfo struct {
	device          uint32
	subdevice       uint32
	stream          int32
	card            int32
	id              [64]byte
	name            [80]byte
	subname         [32]byte
	devClass        int32
	devSubclass     int32
	subdevicesCount uint32
	subdevicesAvail uint32
	sync            [16]byte
	reserved        [64]byte
}
