//go:build !windows

package rt

// RaiseProcessPriority is the Windows process-wide scheduler-lock mitigation.
func RaiseProcessPriority() (restore func() error, priority string, err error) {
	return func() error { return nil }, "", ErrUnsupported
}
