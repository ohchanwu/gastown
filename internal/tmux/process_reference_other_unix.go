//go:build !windows && !darwin && !linux

package tmux

func retainProcessTree(int) ([]retainedProcess, error) {
	return nil, ErrProcessReferenceUnsupported
}
