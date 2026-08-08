//go:build !windows && !darwin && !linux

package tmux

import "context"

func retainProcessTree(int) ([]retainedProcess, error) {
	return nil, ErrProcessReferenceUnsupported
}

func stabilizeProcessTree(context.Context, int, []retainedProcess) ([]retainedProcess, error) {
	return nil, ErrProcessReferenceUnsupported
}
