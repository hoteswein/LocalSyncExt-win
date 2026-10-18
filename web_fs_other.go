//go:build !windows

package main

func listRoots() []string { return []string{"/"} }
