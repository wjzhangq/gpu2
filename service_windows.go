//go:build windows
// +build windows

package main

import (
	"log"

	"golang.org/x/sys/windows/svc"
)

type appService struct{}

func (s *appService) Execute(args []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	go main_func()
	status <- svc.Status{State: svc.Running}

	for c := range r {
		switch c.Cmd {
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 0
}

func tryRunAsWindowsService() {
	is, err := svc.IsWindowsService()
	if err == nil && is {
		log.Println("Running as Windows service...")
		svc.Run("GPUService", &appService{})
		return
	}

	log.Println("Running as normal program in Windows")
	main_func()
}
