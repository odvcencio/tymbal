//go:build linux && (amd64 || arm64)

package alsa

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const (
	pcmPlayback = 0
	pcmCapture  = 1
)

type endpoint struct {
	cardNumber int
	device     int
	cardID     string
	cardName   string
	pcmName    string
	playback   bool
	capture    bool
}

func (e endpoint) id() string { return fmt.Sprintf("hw:%s,%d", e.cardID, e.device) }

func (e endpoint) pcmPath(capture bool) string {
	suffix := "p"
	if capture {
		suffix = "c"
	}
	return fmt.Sprintf("/dev/snd/pcmC%dD%d%s", e.cardNumber, e.device, suffix)
}

func cString(b []byte) string {
	for i, v := range b {
		if v == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func controlPaths() ([]string, error) {
	paths, err := filepath.Glob("/dev/snd/controlC[0-9]*")
	if err != nil {
		return nil, err
	}
	return paths, nil
}

func discoverEndpoints() ([]endpoint, error) {
	paths, err := controlPaths()
	if err != nil {
		return nil, err
	}
	var out []endpoint
	for _, path := range paths {
		cardNumber, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "controlC"))
		if err != nil {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // device disappeared between glob and open
			}
			return nil, fmt.Errorf("alsa: open %s: %w", path, err)
		}
		var card sndCtlCardInfo
		err = ioctl(int(file.Fd()), ioctlCtlCardInfo, unsafe.Pointer(&card))
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("alsa: card info %s: %w", path, err)
		}
		cardID := cString(card.id[:])
		if cardID == "" {
			cardID = strconv.Itoa(cardNumber)
		}
		for dev := int32(-1); ; {
			if err := ioctl(int(file.Fd()), ioctlCtlPCMNextDevice, unsafe.Pointer(&dev)); err != nil {
				_ = file.Close()
				return nil, fmt.Errorf("alsa: next PCM on %s: %w", path, err)
			}
			if dev < 0 {
				break
			}
			e := endpoint{cardNumber: cardNumber, device: int(dev), cardID: cardID, cardName: cString(card.name[:])}
			for _, direction := range []int32{pcmPlayback, pcmCapture} {
				info := sndPCMInfo{device: uint32(dev), stream: direction}
				if err := ioctl(int(file.Fd()), ioctlCtlPCMInfo, unsafe.Pointer(&info)); err != nil {
					if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENODEV) {
						continue
					}
					_ = file.Close()
					return nil, fmt.Errorf("alsa: PCM info %s device %d: %w", path, dev, err)
				}
				if e.pcmName == "" {
					e.pcmName = cString(info.name[:])
				}
				if direction == pcmPlayback {
					e.playback = true
				} else {
					e.capture = true
				}
			}
			if e.playback || e.capture {
				out = append(out, e)
			}
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
