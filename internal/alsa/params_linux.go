//go:build linux && (amd64 || arm64)

package alsa

import (
	"errors"
	"fmt"
	"math"
	"unsafe"

	"m31labs.dev/tymbal/internal/format"
)

const (
	hwParamAccess     = 0
	hwParamFormat     = 1
	hwParamSubformat  = 2
	hwParamChannels   = 10
	hwParamRate       = 11
	hwParamPeriodSize = 13
	hwParamPeriods    = 15
	hwParamBufferSize = 17

	pcmAccessRWInterleaved = 3
	pcmSubformatStandard   = 0

	pcmFormatS16LE   = 2
	pcmFormatS32LE   = 10
	pcmFormatFloatLE = 14
	pcmFormatS24_3LE = 32

	sndPCMVersion = 0x020011 // SNDRV_PCM_VERSION from the local asound.h.
)

var errUnsupportedPCMParams = errors.New("alsa: requested PCM parameters are unsupported")

type pcmParams struct {
	rate, channels, period, periods int
	format                          format.Format
	formatCode                      uint32
	bufferFrames, bufferBytes       uint64
	hw                              hwParams
	sw                              swParams
}

// hwParamsAnything builds the kernel's unconstrained parameter request:
// all public masks and intervals are open, and every parameter is requested.
func hwParamsAnything() hwParams {
	var p hwParams
	for i := range p.masks {
		for j := range p.masks[i].bits {
			p.masks[i].bits[j] = math.MaxUint32
		}
	}
	for i := range p.intervals {
		p.intervals[i] = sndInterval{min: 0, max: math.MaxUint32}
	}
	p.rmask = (1 << 20) - 1 // HW parameters are numbered from 0 through 19.
	return p
}

func maskIndex(param int) (int, bool) {
	if param < hwParamAccess || param > hwParamSubformat {
		return 0, false
	}
	return param - hwParamAccess, true
}

func intervalIndex(param int) (int, bool) {
	if param < 8 || param > 19 {
		return 0, false
	}
	return param - 8, true
}

// hwSetMask constrains a mask parameter to one value and requests that
// parameter in the next HW_REFINE call.
func hwSetMask(p *hwParams, param, value int) bool {
	i, ok := maskIndex(param)
	if !ok || value < 0 || value >= 256 {
		return false
	}
	p.masks[i] = sndMask{}
	p.masks[i].bits[value/32] = uint32(1) << uint(value%32)
	p.rmask |= 1 << uint(param)
	return true
}

// hwMaskHas reports whether a refined mask allows value.
func hwMaskHas(p hwParams, param, value int) bool {
	i, ok := maskIndex(param)
	if !ok || value < 0 || value >= 256 {
		return false
	}
	return p.masks[i].bits[value/32]&(uint32(1)<<uint(value%32)) != 0
}

// hwSetInterval constrains an interval parameter. Integer marks the requested
// interval as integral, as required for channel/rate/period geometry.
func hwSetInterval(p *hwParams, param int, min, max uint32, integer bool) bool {
	i, ok := intervalIndex(param)
	if !ok || min > max {
		return false
	}
	flags := uint32(0)
	if integer {
		flags |= intervalInteger
	}
	p.intervals[i] = sndInterval{min: min, max: max, flags: flags}
	p.rmask |= 1 << uint(param)
	return true
}

// hwIntervalRange returns the kernel-refined inclusive range and flags.
func hwIntervalRange(p hwParams, param int) (min, max, flags uint32, ok bool) {
	i, ok := intervalIndex(param)
	if !ok {
		return 0, 0, 0, false
	}
	v := p.intervals[i]
	return v.min, v.max, v.flags, v.flags&intervalEmpty == 0 && v.min <= v.max
}

// refinePCM applies the current constraints through SNDRV_PCM_IOCTL_HW_REFINE.
func refinePCM(fd int, p *hwParams) error {
	p.cmask = 0
	if err := ioctl(fd, ioctlPCMHWRefine, unsafe.Pointer(p)); err != nil {
		return err
	}
	return nil
}

func validateRefined(p hwParams) error {
	for i, m := range p.masks {
		allZero := true
		for _, word := range m.bits {
			if word != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			return fmt.Errorf("%w: mask parameter %d became empty", errUnsupportedPCMParams, i)
		}
	}
	for i, v := range p.intervals {
		if v.flags&intervalEmpty != 0 || v.min > v.max {
			return fmt.Errorf("%w: interval parameter %d became empty", errUnsupportedPCMParams, i+8)
		}
	}
	return nil
}

func intervalMinimum(v sndInterval) (uint32, bool) {
	if v.flags&intervalEmpty != 0 || v.min > v.max {
		return 0, false
	}
	min := v.min
	if v.flags&intervalOpenMin != 0 {
		if min == math.MaxUint32 {
			return 0, false
		}
		min++
	}
	if min > v.max || (min == v.max && v.flags&intervalOpenMax != 0) {
		return 0, false
	}
	return min, true
}

func makeSWParams(period, bufferFrames uint64) swParams {
	return swParams{
		tstampMode:       1, // SNDRV_PCM_TSTAMP_ENABLE
		periodStep:       1,
		availMin:         period,
		xferAlign:        1,
		startThreshold:   bufferFrames,
		stopThreshold:    bufferFrames,
		silenceThreshold: 0,
		silenceSize:      0,
		tstampType:       1, // SNDRV_PCM_TSTAMP_TYPE_MONOTONIC
	}
}

func hwIntervalMinimum(p hwParams, param int) (uint32, bool) {
	i, ok := intervalIndex(param)
	if !ok {
		return 0, false
	}
	return intervalMinimum(p.intervals[i])
}

func hwParamsEmpty(p hwParams) bool {
	for _, m := range p.masks {
		for _, word := range m.bits {
			if word != 0 {
				goto nonemptyMask
			}
		}
		return true
	nonemptyMask:
	}
	for _, v := range p.intervals {
		if v.flags&intervalEmpty != 0 || v.min > v.max {
			return true
		}
	}
	return false
}

func unsupported(param string, requested int) error {
	return fmt.Errorf("%w: %s=%d", errUnsupportedPCMParams, param, requested)
}

type pcmFormatChoice struct {
	code   uint32
	format format.Format
}

var pcmFormatPreference = [...]pcmFormatChoice{
	{pcmFormatFloatLE, format.F32LE},
	{pcmFormatS32LE, format.S32LE},
	{pcmFormatS24_3LE, format.S24_3LE},
	{pcmFormatS16LE, format.S16LE},
}

func supportedPCMFormatCodes(p hwParams) []uint32 {
	var codes []uint32
	for code := 0; code < 256; code++ {
		if hwMaskHas(p, hwParamFormat, code) {
			codes = append(codes, uint32(code))
		}
	}
	return codes
}

// negotiatePCM queries, constrains, and commits one PCM direction. It then
// configures kernel-managed wake, automatic start/stop, and monotonic stamps.
func negotiatePCM(fd, rate, channels, period, periods int) (pcmParams, error) {
	if rate <= 0 || channels <= 0 || period <= 0 || periods <= 0 {
		return pcmParams{}, fmt.Errorf("%w: non-positive request", errUnsupportedPCMParams)
	}
	if uint64(period) > math.MaxUint32 || uint64(rate) > math.MaxUint32 || uint64(channels) > math.MaxUint32 || uint64(periods) > math.MaxUint32 {
		return pcmParams{}, fmt.Errorf("%w: request exceeds ALSA parameter range", errUnsupportedPCMParams)
	}

	p := hwParamsAnything()
	if err := refinePCM(fd, &p); err != nil {
		return pcmParams{}, fmt.Errorf("ALSA HW_REFINE (any): %w", err)
	}
	if err := validateRefined(p); err != nil {
		return pcmParams{}, err
	}

	refine := func(parameter string, requested int) error {
		if err := refinePCM(fd, &p); err != nil {
			return fmt.Errorf("ALSA HW_REFINE (%s): %w", parameter, err)
		}
		return validateRefined(p)
	}
	constrainMask := func(param, value int, name string) error {
		if !hwSetMask(&p, param, value) {
			return unsupported(name, value)
		}
		if err := refine(name, value); err != nil {
			return err
		}
		if !hwMaskHas(p, param, value) {
			return unsupported(name, value)
		}
		return nil
	}
	constrainInterval := func(param, value int, name string) error {
		if !hwSetInterval(&p, param, uint32(value), uint32(value), true) {
			return unsupported(name, value)
		}
		if err := refine(name, value); err != nil {
			return err
		}
		actual, ok := hwIntervalMinimum(p, param)
		if !ok || actual != uint32(value) {
			return unsupported(name, value)
		}
		return nil
	}

	if err := constrainMask(hwParamAccess, pcmAccessRWInterleaved, "access"); err != nil {
		return pcmParams{}, err
	}
	var chosen pcmFormatChoice
	for _, candidate := range pcmFormatPreference {
		if hwMaskHas(p, hwParamFormat, int(candidate.code)) {
			chosen = candidate
			break
		}
	}
	if chosen.format == "" {
		return pcmParams{}, fmt.Errorf("%w: device format codes are %v; Tymbal supports F32LE, S32LE, S24_3LE, and S16LE", errUnsupportedPCMParams, supportedPCMFormatCodes(p))
	}
	if err := constrainMask(hwParamFormat, int(chosen.code), "format"); err != nil {
		return pcmParams{}, err
	}
	if err := constrainMask(hwParamSubformat, pcmSubformatStandard, "subformat"); err != nil {
		return pcmParams{}, err
	}
	if err := constrainInterval(hwParamChannels, channels, "channels"); err != nil {
		return pcmParams{}, err
	}
	if err := constrainInterval(hwParamRate, rate, "rate"); err != nil {
		return pcmParams{}, err
	}

	if !hwSetInterval(&p, hwParamPeriodSize, uint32(period), math.MaxUint32, true) {
		return pcmParams{}, unsupported("period size", period)
	}
	if err := refine("period size", period); err != nil {
		return pcmParams{}, err
	}
	periodValue, ok := hwIntervalMinimum(p, hwParamPeriodSize)
	if !ok || periodValue < uint32(period) {
		return pcmParams{}, unsupported("period size", period)
	}
	selectedPeriod := periodValue
	if !hwSetInterval(&p, hwParamPeriodSize, selectedPeriod, selectedPeriod, true) {
		return pcmParams{}, unsupported("period size", int(selectedPeriod))
	}
	if err := refine("period size", int(selectedPeriod)); err != nil {
		return pcmParams{}, err
	}
	periodValue, ok = hwIntervalMinimum(p, hwParamPeriodSize)
	if !ok || periodValue != selectedPeriod {
		return pcmParams{}, unsupported("period size", int(selectedPeriod))
	}
	if err := constrainInterval(hwParamPeriods, periods, "periods"); err != nil {
		return pcmParams{}, err
	}

	// USER_PVERSION is set before HW_PARAMS so the kernel interprets all fields
	// with the documented 64-bit PCM protocol version.
	userVersion := int32(sndPCMVersion)
	if err := ioctl(fd, ioctlPCMUserPVersion, unsafe.Pointer(&userVersion)); err != nil {
		return pcmParams{}, fmt.Errorf("ALSA USER_PVERSION: %w", err)
	}
	if err := ioctl(fd, ioctlPCMHWParams, unsafe.Pointer(&p)); err != nil {
		return pcmParams{}, fmt.Errorf("ALSA HW_PARAMS: %w", err)
	}
	if hwParamsEmpty(p) {
		return pcmParams{}, fmt.Errorf("ALSA HW_PARAMS returned an empty parameter set")
	}
	if !hwMaskHas(p, hwParamFormat, int(chosen.code)) {
		return pcmParams{}, fmt.Errorf("ALSA HW_PARAMS returned a different sample format")
	}

	actualRate, ok := hwIntervalMinimum(p, hwParamRate)
	if !ok || actualRate == 0 {
		return pcmParams{}, fmt.Errorf("ALSA HW_PARAMS returned an invalid rate")
	}
	actualChannels, ok := hwIntervalMinimum(p, hwParamChannels)
	if !ok || actualChannels == 0 {
		return pcmParams{}, fmt.Errorf("ALSA HW_PARAMS returned invalid channel count")
	}
	actualPeriod, ok := hwIntervalMinimum(p, hwParamPeriodSize)
	if !ok || actualPeriod == 0 {
		return pcmParams{}, fmt.Errorf("ALSA HW_PARAMS returned an invalid period size")
	}
	actualPeriods, ok := hwIntervalMinimum(p, hwParamPeriods)
	if !ok || actualPeriods == 0 {
		return pcmParams{}, fmt.Errorf("ALSA HW_PARAMS returned an invalid period count")
	}
	bufferFrames32, ok := hwIntervalMinimum(p, hwParamBufferSize)
	bufferFrames := uint64(bufferFrames32)
	if !ok || bufferFrames == 0 {
		bufferFrames = uint64(actualPeriod) * uint64(actualPeriods)
	}
	bps := uint64(format.BytesPerSample(chosen.format))
	if bps == 0 || uint64(actualChannels) > math.MaxUint64/bps || bufferFrames > math.MaxUint64/uint64(actualChannels)/bps {
		return pcmParams{}, fmt.Errorf("ALSA HW_PARAMS returned overflowing buffer geometry")
	}
	bufferBytes := bufferFrames * uint64(actualChannels) * bps
	if actualRate > math.MaxInt32 || actualChannels > math.MaxInt32 || actualPeriod > math.MaxInt32 || actualPeriods > math.MaxInt32 {
		return pcmParams{}, fmt.Errorf("ALSA HW_PARAMS returned values outside supported ranges")
	}

	sp := makeSWParams(uint64(actualPeriod), bufferFrames)
	if err := ioctl(fd, ioctlPCMSWParams, unsafe.Pointer(&sp)); err != nil {
		return pcmParams{}, fmt.Errorf("ALSA SW_PARAMS: %w", err)
	}

	return pcmParams{
		rate:         int(actualRate),
		channels:     int(actualChannels),
		period:       int(actualPeriod),
		periods:      int(actualPeriods),
		format:       chosen.format,
		formatCode:   chosen.code,
		bufferFrames: bufferFrames,
		bufferBytes:  bufferBytes,
		hw:           p,
		sw:           sp,
	}, nil
}
