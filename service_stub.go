//go:build !windows
// +build !windows

package main

// Linux/macOS 下啥也不做，直接 startApp()
func tryRunAsWindowsService() {
	main_func()
}
