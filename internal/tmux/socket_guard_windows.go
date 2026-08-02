//go:build windows

package tmux

import "context"

func (t *Tmux) ensureNewSessionSocketSafe() error {
	return nil
}

func (t *Tmux) ensureNewSessionSocketSafeContext(context.Context) error {
	return nil
}
