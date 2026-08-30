package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"

	connect "github.com/Ali-Nigzu/gateway/connect"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath, err := cameraConfigPath()
	if err != nil {
		return fail(err)
	}

	config, err := connect.LoadConfig(configPath)
	if err != nil {
		return fail(err)
	}
	fmt.Println("Config loaded")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := connect.Connect(ctx, config); err != nil {
		return fail(err)
	}
	fmt.Println("PASS")
	return 0
}

func cameraConfigPath() (string, error) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("camera config invalid: unable to locate camera.yml")
	}
	connectDirectory := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	absoluteDirectory, err := filepath.Abs(connectDirectory)
	if err != nil {
		return "", errors.New("camera config invalid: unable to locate camera.yml")
	}
	return filepath.Join(absoluteDirectory, "camera.yml"), nil
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
	return 1
}
