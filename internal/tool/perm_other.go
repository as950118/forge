//go:build !unix

package tool

func checkPrivate(string, bool) error { return nil }
