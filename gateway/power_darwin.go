//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation

#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOMessage.h>
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <stdatomic.h>
#include <stdlib.h>

typedef struct camos_power_watcher {
	io_connect_t root_port;
	IONotificationPortRef notification_port;
	io_object_t notifier;
	CFRunLoopRef run_loop;
	_Atomic int resume_pending;
	_Atomic int stopping;
} camos_power_watcher;

static void camos_power_callback(
	void *reference,
	io_service_t service,
	natural_t message_type,
	void *message_argument
) {
	camos_power_watcher *watcher = (camos_power_watcher *)reference;

	switch (message_type) {
	case kIOMessageCanSystemSleep:
	case kIOMessageSystemWillSleep:
		IOAllowPowerChange(watcher->root_port, (long)message_argument);
		break;
	case kIOMessageSystemHasPoweredOn:
		atomic_store_explicit(&watcher->resume_pending, 1, memory_order_release);
		break;
	default:
		break;
	}
}

static camos_power_watcher *camos_power_create(void) {
	camos_power_watcher *watcher = calloc(1, sizeof(camos_power_watcher));
	if (watcher == NULL) {
		return NULL;
	}

	watcher->run_loop = CFRunLoopGetCurrent();
	CFRetain(watcher->run_loop);
	watcher->root_port = IORegisterForSystemPower(
		watcher,
		&watcher->notification_port,
		camos_power_callback,
		&watcher->notifier
	);
	if (watcher->root_port == MACH_PORT_NULL || watcher->notification_port == NULL) {
		if (watcher->root_port != MACH_PORT_NULL) {
			IOServiceClose(watcher->root_port);
		}
		CFRelease(watcher->run_loop);
		free(watcher);
		return NULL;
	}

	CFRunLoopAddSource(
		watcher->run_loop,
		IONotificationPortGetRunLoopSource(watcher->notification_port),
		kCFRunLoopCommonModes
	);
	return watcher;
}

static int camos_power_wait(camos_power_watcher *watcher) {
	while (!atomic_load_explicit(&watcher->stopping, memory_order_acquire)) {
		if (atomic_exchange_explicit(&watcher->resume_pending, 0, memory_order_acq_rel)) {
			return 1;
		}
		CFRunLoopRunInMode(kCFRunLoopDefaultMode, 1.0, true);
	}
	return 0;
}

static void camos_power_stop(camos_power_watcher *watcher) {
	atomic_store_explicit(&watcher->stopping, 1, memory_order_release);
	CFRunLoopStop(watcher->run_loop);
	CFRunLoopWakeUp(watcher->run_loop);
}

static void camos_power_destroy(camos_power_watcher *watcher) {
	CFRunLoopRemoveSource(
		watcher->run_loop,
		IONotificationPortGetRunLoopSource(watcher->notification_port),
		kCFRunLoopCommonModes
	);
	IODeregisterForSystemPower(&watcher->notifier);
	IOServiceClose(watcher->root_port);
	IONotificationPortDestroy(watcher->notification_port);
	CFRelease(watcher->run_loop);
	free(watcher);
}
*/
import "C"

import (
	"errors"
	"runtime"
)

type powerResumeWatcher struct {
	native *C.camos_power_watcher
	done   <-chan struct{}
}

type powerResumeWatcherStart struct {
	native *C.camos_power_watcher
	err    error
}

func startPowerResumeWatcher(resume chan<- struct{}) (*powerResumeWatcher, error) {
	started := make(chan powerResumeWatcherStart, 1)
	done := make(chan struct{})

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)

		native := C.camos_power_create()
		if native == nil {
			started <- powerResumeWatcherStart{err: errors.New("IORegisterForSystemPower failed")}
			return
		}
		started <- powerResumeWatcherStart{native: native}
		defer C.camos_power_destroy(native)

		for C.camos_power_wait(native) != 0 {
			select {
			case resume <- struct{}{}:
			default:
			}
		}
	}()

	start := <-started
	if start.err != nil {
		<-done
		return nil, start.err
	}
	return &powerResumeWatcher{native: start.native, done: done}, nil
}

func (watcher *powerResumeWatcher) stop() {
	C.camos_power_stop(watcher.native)
	<-watcher.done
}
