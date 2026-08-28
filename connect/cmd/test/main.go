package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	connect "github.com/Ali-Nigzu/gateway/connect"
)

const connectTimeout = 60 * time.Second

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

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

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
