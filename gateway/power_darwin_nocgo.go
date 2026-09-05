//go:build darwin && !cgo

package main

import "errors"

type powerResumeWatcher struct{}

func startPowerResumeWatcher(chan<- struct{}) (*powerResumeWatcher, error) {
	return nil, errors.New("macOS power notifications require a native CGo build")
}

func (*powerResumeWatcher) stop() {}
