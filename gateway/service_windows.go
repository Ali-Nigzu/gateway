//go:build windows

package main

import (
	"context"
	"errors"
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
	serviceExitEmbeddedPayload      = 6
	serviceExitRuntimeIdentity      = 7
	serviceExitLifecycleHandoff     = 8
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

	// Committed local removal outranks every optional runtime dependency,
	// including the FFmpeg process job. A job-object failure must never prevent
	// deletion continuation from being attempted.
	removalPending, err := windowsRemovalAuthorized()
	if err != nil {
		return true, serviceExitRuntimeIdentity
	}
	if removalPending {
		if err := beginGatewayRemoval(); err != nil {
			return true, serviceExitLifecycleHandoff
		}
		// The transient helper has synchronously acknowledged readiness and waits
		// for this service process to exit. Report an orderly stop so SCM does not
		// queue a concurrent 30-second recovery restart while that helper removes
		// the package. The helper explicitly restarts this service on any failure.
		return windowsServiceResultForHandoff(controllerExitLifecycleHandoff, true)
	}
	gatewayID, err := loadGatewayID()
	if err != nil {
		return true, serviceExitGatewayIDUnavailable
	}
	handoff, startupUpdateFailure := reconcileUpdateStateAtStartup(gatewayID)
	if handoff {
		return windowsServiceResult(controllerExitLifecycleHandoff)
	}
	recoverStartupFailure := func(failure error, exitCode uint32) (bool, uint32) {
		recoveryHandoff, recoveryErr := recoverCandidateAfterStartupFailure(gatewayID, failure)
		if errors.Is(recoveryErr, errTerminalRemovalCommitted) {
			if err := beginGatewayRemoval(); err == nil {
				return windowsServiceResultForHandoff(controllerExitLifecycleHandoff, true)
			}
			return windowsServiceResult(controllerExitLifecycleHandoff)
		}
		if recoveryHandoff {
			return windowsServiceResult(controllerExitLifecycleHandoff)
		}
		return true, exitCode
	}

	processJob, exitCode := createProcessLifetimeJob()
	if exitCode != 0 {
		return recoverStartupFailure(errors.New("Gateway process job startup failed"), exitCode)
	}
	// KILL_ON_JOB_CLOSE makes this handle process-lifetime state. The operating
	// system closes it as this service process exits; closing it earlier would
	// terminate the service itself together with any remaining FFmpeg children.
	service.processJob = processJob

	if err := clearStaleFramePackageState(); err != nil {
		return recoverStartupFailure(err, serviceExitRuntimeIdentity)
	}
	ffmpegPath, err := installedFFmpegPath()
	if err != nil {
		return recoverStartupFailure(err, serviceExitEmbeddedPayload)
	}
	if err := ensureEmbeddedFFmpeg(ffmpegPath); err != nil {
		return recoverStartupFailure(err, serviceExitEmbeddedPayload)
	}

	ctx, cancel := context.WithCancel(context.Background())
	credentials, err := newRuntimeCredentials(ctx)
	if err != nil {
		cancel()
		return recoverStartupFailure(err, serviceExitRuntimeIdentity)
	}
	if credentials.gatewayID != gatewayID {
		cancel()
		return recoverStartupFailure(
			errors.New("GatewayID changed during runtime startup"),
			serviceExitRuntimeIdentity,
		)
	}
	resume := make(chan struct{}, 1)
	controllerDone := make(chan controllerExit, 1)
	go func() {
		controllerDone <- runController(ctx, credentials, resume, startupUpdateFailure)
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

		case exit := <-controllerDone:
			cancel()
			if exit == controllerExitLifecycleHandoff {
				removalPending, removalErr := windowsRemovalAuthorized()
				if removalErr == nil && removalPending {
					// As above, a committed removal with an acknowledged helper is
					// an orderly ownership transfer, not a service failure. Update
					// and restart handoffs still use SCM recovery below.
					return windowsServiceResultForHandoff(exit, true)
				}
			}
			return windowsServiceResult(exit)
		}
	}
}

func windowsServiceResult(exit controllerExit) (bool, uint32) {
	switch exit {
	case controllerExitLifecycleHandoff:
		// Non-crash recovery is configured by the installer. A recoverable
		// nonzero result protects the small window after a transient helper has
		// acknowledged readiness but before it completes the lifecycle action.
		return true, serviceExitLifecycleHandoff
	default:
		return true, serviceExitControllerStopped
	}
}

func windowsServiceResultForHandoff(
	exit controllerExit,
	removalAuthorized bool,
) (bool, uint32) {
	if exit == controllerExitLifecycleHandoff && removalAuthorized {
		return false, 0
	}
	return windowsServiceResult(exit)
}

func createProcessLifetimeJob() (windows.Handle, uint32) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, serviceExitJobCreationFailed
	}

	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags =
		windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
			windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
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
