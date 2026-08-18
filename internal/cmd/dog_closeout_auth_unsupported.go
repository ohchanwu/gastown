//go:build !linux && !windows

package cmd

func dogCloseoutFinalizerAuthorized(encoded string) bool {
	return dogCloseoutHostSessionAuthorized(encoded)
}
