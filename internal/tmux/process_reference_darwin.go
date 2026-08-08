//go:build darwin

package tmux

func retainProcessTree(int) ([]retainedProcess, error) {
	return nil, ErrProcessReferenceUnsupported
}
