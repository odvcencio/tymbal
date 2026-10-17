//go:build darwin && (amd64 || arm64)

package tymbal

import "m31labs.dev/tymbal/internal/coreaudio"

// Hosts returns the macOS audio backends in preference order.
//
// The coreaudio driver talks to Core Audio and AudioToolbox through the
// Go runtime's cgo_import_dynamic mechanism (no cgo, no purego, no external
// dependencies). The render callback is a C-ABI assembly function that
// never enters the Go runtime; it copies from an mmap'd ring and signals
// a Mach semaphore. See internal/coreaudio for the implementation.
func Hosts() []Host { return []Host{{d: coreaudio.New()}} }
