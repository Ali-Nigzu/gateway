//go:build windows

package main

import (
	"context"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

const (
	ffmpegExecutableName = "ffmpeg.exe"

	pbtAPMResumeAutomatic = 0x0012

	serviceExitGatewayIDUnavailable = 1
	serviceExitJobCreationFailed    = 2
	serviceExitJobConfiguration     = 3
	serviceExitJobAssignment        = 4
	serviceExitControllerStopped    = 5
)

type camOSWindowsService struct {
	processJob windows.Handle
}

func runService() error {
	return svc.Run(windowsServiceName, &camOSWindowsService{})
}

func (service *camOSWindowsService) Execute(
	_ []string,
	requests <-chan svc.ChangeRequest,
	statusChanges chan<- svc.Status,
) (bool, uint32) {
	statusChanges <- svc.Status{
		State:    svc.StartPending,
		WaitHint: 30_000,
	}

	gatewayID, err := loadGatewayID()
	if err != nil {
		return true, serviceExitGatewayIDUnavailable
	}

	processJob, exitCode := createProcessLifetimeJob()
	if exitCode != 0 {
		return true, exitCode
	}
	// KILL_ON_JOB_CLOSE makes this handle process-lifetime state. The operating
	// system closes it as this service process exits; closing it earlier would
	// terminate the service itself together with any remaining FFmpeg children.
	service.processJob = processJob

	ctx, cancel := context.WithCancel(context.Background())
	resume := make(chan struct{}, 1)
	controllerDone := make(chan struct{})
	go func() {
		runController(ctx, gatewayID, resume)
		close(controllerDone)
	}()

	runningStatus := svc.Status{
		State:   svc.Running,
		Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPowerEvent,
	}
	statusChanges <- runningStatus

	for {
		select {
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				statusChanges <- runningStatus

			case svc.PowerEvent:
				if request.EventType == pbtAPMResumeAutomatic {
					select {
					case resume <- struct{}{}:
					default:
					}
				}

			case svc.Stop, svc.Shutdown:
				statusChanges <- svc.Status{
					State:    svc.StopPending,
					WaitHint: 120_000,
				}
				cancel()
				<-controllerDone
				return false, 0
			}

		case <-controllerDone:
			cancel()
			return true, serviceExitControllerStopped
		}
	}
}

func createProcessLifetimeJob() (windows.Handle, uint32) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, serviceExitJobCreationFailed
	}

	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	); err != nil {
		windows.CloseHandle(job)
		return 0, serviceExitJobConfiguration
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return 0, serviceExitJobAssignment
	}
	return job, 0
}
