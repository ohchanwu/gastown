//go:build !linux

package tmux

func sessionCustodyLaunchSupported() bool { return false }

func runSessionWithCustody(string, string) error {
	return ErrSessionCustodyUnsupported
}

func runSessionCustodyInit() (bool, error) { return false, nil }

func retainSessionCustody(string, int) (sessionCustodyHandle, error) {
	return nil, ErrSessionCustodyUnsupported
}
