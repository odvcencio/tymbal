//go:build linux && (amd64 || arm64)

package alsa

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"syscall"
	"time"

	"m31labs.dev/tymbal/internal/driver"
)

type backend struct{}

// New constructs the direct ALSA hardware backend. It does not open a device.
func New() driver.Driver { return backend{} }

func (backend) Name() string { return "alsa" }

func (backend) Devices() ([]driver.Info, error) {
	endpoints, err := discoverEndpoints()
	if err != nil {
		return nil, err
	}
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].cardNumber != endpoints[j].cardNumber {
			return endpoints[i].cardNumber < endpoints[j].cardNumber
		}
		return endpoints[i].device < endpoints[j].device
	})
	devices := make([]driver.Info, 0, len(endpoints))
	var defaultOutput, defaultInput bool
	for _, endpoint := range endpoints {
		info := driver.Info{ID: endpoint.id(), Name: endpoint.pcmName, Exclusive: true}
		if info.Name == "" {
			info.Name = endpoint.cardName
		}
		if endpoint.playback {
			capability, err := probePCM(endpoint.pcmPath(false))
			if err != nil {
				if errors.Is(err, syscall.EBUSY) {
					return nil, fmt.Errorf("%w: ALSA playback %s", driver.ErrBusy, info.ID)
				}
				return nil, fmt.Errorf("alsa: probe playback %s: %w", info.ID, err)
			}
			info.Outputs = capability.channels
			info.MinPeriod, info.MaxPeriod = capability.minPeriod, capability.maxPeriod
			info.SampleRates = append(info.SampleRates, capability.rates...)
			if !defaultOutput {
				info.Default |= 1 // driver direction Output
				defaultOutput = true
			}
		}
		if endpoint.capture {
			capability, err := probePCM(endpoint.pcmPath(true))
			if err != nil {
				if errors.Is(err, syscall.EBUSY) {
					return nil, fmt.Errorf("%w: ALSA capture %s", driver.ErrBusy, info.ID)
				}
				return nil, fmt.Errorf("alsa: probe capture %s: %w", info.ID, err)
			}
			info.Inputs = capability.channels
			if info.MinPeriod == 0 || capability.minPeriod < info.MinPeriod {
				info.MinPeriod = capability.minPeriod
			}
			if capability.maxPeriod > info.MaxPeriod {
				info.MaxPeriod = capability.maxPeriod
			}
			for _, rate := range capability.rates {
				found := false
				for _, present := range info.SampleRates {
					if rate == present {
						found = true
						break
					}
				}
				if !found {
					info.SampleRates = append(info.SampleRates, rate)
				}
			}
			if !defaultInput {
				info.Default |= 2 // driver direction Input
				defaultInput = true
			}
		}
		devices = append(devices, info)
	}
	return devices, nil
}

func (b backend) Default(dir uint8) (driver.Info, error) {
	if dir != 1 && dir != 2 {
		return driver.Info{}, fmt.Errorf("alsa: invalid direction %d", dir)
	}
	devices, err := b.Devices()
	if err != nil {
		return driver.Info{}, err
	}
	for _, d := range devices {
		if d.Default&dir != 0 {
			return d, nil
		}
	}
	return driver.Info{}, fmt.Errorf("alsa: no default device for direction %d", dir)
}

// Watch polls card-control metadata off the audio path. It does not open PCM
// endpoints, which can be busy while an active stream is running.
func (backend) Watch(fn func(driver.Event)) (stop func()) {
	if fn == nil {
		return func() {}
	}
	stopCh := make(chan struct{})
	var once sync.Once
	go func() {
		previous, _ := discoverEndpoints()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				current, err := discoverEndpoints()
				if err != nil {
					continue
				}
				for _, old := range previous {
					if !hasEndpoint(current, old.id()) {
						fn(driver.Event{Kind: 2, Device: endpointEventInfo(old)}) // DeviceRemoved
					}
				}
				for _, next := range current {
					if !hasEndpoint(previous, next.id()) {
						fn(driver.Event{Kind: 1, Device: endpointEventInfo(next)}) // DeviceAdded
					}
				}
				previous = current
			}
		}
	}()
	return func() { once.Do(func() { close(stopCh) }) }
}

func hasEndpoint(endpoints []endpoint, id string) bool {
	for _, e := range endpoints {
		if e.id() == id {
			return true
		}
	}
	return false
}

func endpointEventInfo(e endpoint) driver.Info {
	name := e.pcmName
	if name == "" {
		name = e.cardName
	}
	info := driver.Info{ID: e.id(), Name: name, Exclusive: true}
	if e.playback {
		info.Outputs = 1 // at least one; re-enumerate for negotiated maximum
	}
	if e.capture {
		info.Inputs = 1
	}
	return info
}

type pcmCapability struct {
	channels             int
	minPeriod, maxPeriod int
	rates                []int
}

func probePCM(path string) (pcmCapability, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return pcmCapability{}, err
	}
	defer syscall.Close(fd)
	p := hwParamsAnything()
	if err := refinePCM(fd, &p); err != nil {
		return pcmCapability{}, err
	}
	channelsMin, channelsMax, channelFlags, ok := hwIntervalRange(p, hwParamChannels)
	if !ok || channelsMin == 0 || channelsMax == 0 {
		return pcmCapability{}, fmt.Errorf("alsa: no channel range for %s", path)
	}
	if channelFlags&intervalOpenMax != 0 {
		channelsMax--
	}
	periodMin, periodMax, periodFlags, ok := hwIntervalRange(p, hwParamPeriodSize)
	if !ok || periodMin == 0 || periodMax == 0 {
		return pcmCapability{}, fmt.Errorf("alsa: no period range for %s", path)
	}
	if periodFlags&intervalOpenMin != 0 {
		periodMin++
	}
	if periodFlags&intervalOpenMax != 0 {
		periodMax--
	}
	if channelsMax == 0 || periodMin > periodMax {
		return pcmCapability{}, fmt.Errorf("alsa: empty channel or period range for %s", path)
	}
	capability := pcmCapability{channels: int(channelsMax), minPeriod: int(periodMin), maxPeriod: int(periodMax)}
	for _, rate := range []int{44_100, 48_000, 88_200, 96_000, 176_400, 192_000} {
		candidate := p
		hwSetInterval(&candidate, hwParamRate, uint32(rate), uint32(rate), true)
		if err := refinePCM(fd, &candidate); err == nil {
			minimum, maximum, flags, ok := hwIntervalRange(candidate, hwParamRate)
			if ok && minimum <= uint32(rate) && uint32(rate) <= maximum &&
				!(minimum == uint32(rate) && flags&intervalOpenMin != 0) &&
				!(maximum == uint32(rate) && flags&intervalOpenMax != 0) {
				capability.rates = append(capability.rates, rate)
			}
		}
	}
	return capability, nil
}

func (backend) Open(req driver.Request) (_ driver.Stream, returnedErr error) {
	defer func() {
		if returnedErr == nil {
			return
		}
		switch {
		case errors.Is(returnedErr, syscall.EBUSY):
			returnedErr = fmt.Errorf("%w: %v", driver.ErrBusy, returnedErr)
		case errors.Is(returnedErr, syscall.ENODEV), errors.Is(returnedErr, syscall.ENOENT):
			returnedErr = fmt.Errorf("%w: %v", driver.ErrLost, returnedErr)
		case errors.Is(returnedErr, errUnsupportedPCMParams):
			rates := []int(nil)
			if req.Output != nil {
				rates = req.Output.SampleRates
			} else if req.Input != nil {
				rates = req.Input.SampleRates
			}
			returnedErr = fmt.Errorf("%w: %v (advertised sample rates: %v)", driver.ErrFormat, returnedErr, rates)
		}
	}()
	if req.Output == nil && req.Input == nil {
		return nil, fmt.Errorf("%w: ALSA requires an input or output device", driver.ErrUnsupported)
	}
	endpoints, err := discoverEndpoints()
	if err != nil {
		return nil, err
	}
	var outEndpoint, inEndpoint *endpoint
	for i := range endpoints {
		e := &endpoints[i]
		if req.Output != nil && e.id() == req.Output.ID && e.playback {
			outEndpoint = e
		}
		if req.Input != nil && e.id() == req.Input.ID && e.capture {
			inEndpoint = e
		}
	}
	if req.Output != nil && outEndpoint == nil || req.Input != nil && inEndpoint == nil {
		return nil, fmt.Errorf("alsa: device changed since enumeration: %w", syscall.ENODEV)
	}
	if outEndpoint != nil && inEndpoint != nil && outEndpoint.cardNumber != inEndpoint.cardNumber {
		return nil, fmt.Errorf("%w: ALSA duplex devices must share a card", driver.ErrUnsupported)
	}
	outFD, inFD := -1, -1
	defer func() {
		if outFD >= 0 {
			_ = syscall.Close(outFD)
		}
		if inFD >= 0 {
			_ = syscall.Close(inFD)
		}
	}()
	var outParams, inParams pcmParams
	if outEndpoint != nil {
		outFD, err = syscall.Open(outEndpoint.pcmPath(false), syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		outParams, err = negotiatePCM(outFD, req.SampleRate, req.OutChannels, req.Period, req.Periods)
		if err != nil {
			return nil, err
		}
	}
	if inEndpoint != nil {
		inFD, err = syscall.Open(inEndpoint.pcmPath(true), syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		inParams, err = negotiatePCM(inFD, req.SampleRate, req.InChannels, req.Period, req.Periods)
		if err != nil {
			return nil, err
		}
	}
	stream, err := openPCMStream(outFD, inFD, outParams, inParams)
	if err != nil {
		return nil, err
	}
	outFD, inFD = -1, -1 // stream owns both descriptors
	return stream, nil
}

var _ driver.Driver = backend{}
