package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	var err error
	switch {
	case len(os.Args) == 3 && os.Args[1] == "commission":
		err = commission(os.Args[2])
	case len(os.Args) == 2 && os.Args[1] == "service":
		err = runService()
	default:
		err = errors.New("usage: camos-gateway commission <gateway_id>")
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}
