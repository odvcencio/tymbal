//go:build !linux || (!amd64 && !arm64)

package rt

// RaiseProcessThreads is available on Linux amd64 and arm64 only.
func RaiseProcessThreads(p int) (restore func(), raised int, err error) {
	return func() {}, 0, ErrUnsupported
}
