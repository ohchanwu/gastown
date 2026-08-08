//go:build !linux && !integration

package cmd

func runSessionBrokerReexecHelper() bool { return false }

func runSessionBrokerRawClientHelper() (int, bool) { return 0, false }
