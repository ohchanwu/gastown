//go:build !linux

package cmd

func dogCloseoutFinalizerAuthorized(encoded string) bool {
	return dogCloseoutHostSessionAuthorized(encoded)
}
