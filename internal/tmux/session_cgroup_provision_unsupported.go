//go:build !linux

package tmux

// ProvisionSessionCgroupRoot is a no-op where Linux cgroup containment is not
// used. Unsupported platforms retain their separately reviewed stop path.
func ProvisionSessionCgroupRoot() error { return nil }
