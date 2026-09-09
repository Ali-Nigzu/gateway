package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

func main() {
	retainGatewayReleaseStamp()
	if handled, err := handleInternalPlatformCommand(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
			os.Exit(1)
		}
		return
	}

	var err error
	switch {
	case len(os.Args) == 3 && os.Args[1] == "commission":
		err = commission(os.Args[2])
	case len(os.Args) == 2 && os.Args[1] == "service":
		err = runService()
	case len(os.Args) == 2 && os.Args[1] == "version":
		var filename string
		filename, err = artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
		if err == nil {
			fmt.Printf("BuildVersion=%s Target=%s\n", BuildVersion, filename)
		}
	default:
		err = errors.New("usage: camos-gateway commission <commission_id> | service | version")
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}
