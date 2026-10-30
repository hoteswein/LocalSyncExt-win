//go:build !windows

package main

// Вне Windows защита системой не используется (программа рассчитана на Windows;
// этот вариант нужен для тестов и отладки): значения лежат в файле с правами 0600.
func protectSecret(b []byte) ([]byte, error)   { return append([]byte(nil), b...), nil }
func unprotectSecret(b []byte) ([]byte, error) { return append([]byte(nil), b...), nil }
