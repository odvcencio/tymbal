//go:build linux && (amd64 || arm64)

package tymbal

import "m31labs.dev/tymbal/internal/alsa"

// Hosts returns the Linux audio backends in preference order.
func Hosts() []Host { return []Host{{d: alsa.New()}} }
