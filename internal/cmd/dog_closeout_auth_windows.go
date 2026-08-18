//go:build windows

package cmd

func dogCloseoutFinalizerAuthorized(encoded string) bool {
	return dogCloseoutDetachedHostAuthorized(encoded)
}
