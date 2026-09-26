//go:build !windows && !(linux && (amd64 || arm64))

package tymbal

// Hosts returns the platform audio backends in preference order.
func Hosts() []Host { return nil }
