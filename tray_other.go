//go:build !windows

package main

import "errors"

var errTrayUnsupported = errors.New("значок в трее поддерживается только в Windows")

func runTray(tc *trayCtx) error { return errTrayUnsupported }

func acquireInstance(key string) (bool, func()) { return true, func() {} }

func attachConsole() {}

func showError(msg string) {}

func trayBalloon(title, msg string, level int) {}

func stopTray() {}
