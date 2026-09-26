//go:build windows

package tymbal

import "m31labs.dev/tymbal/internal/wasapi"

// Hosts returns the Windows audio backends in preference order.
func Hosts() []Host { return []Host{{d: wasapi.New()}} }
