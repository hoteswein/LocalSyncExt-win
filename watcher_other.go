//go:build !windows

package main

import "context"

func watchDir(ctx context.Context, dir string, recursive bool, onStart func(), emit func(rel string, overflow bool)) error {
	return errWatchUnsupported
}
