package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch {
	case len(os.Args) == 1:
		var siteID int64
		siteID, err = loadSiteID()
		if err == nil {
			err = startGateway(ctx, siteID)
		}
	case len(os.Args) == 3 && os.Args[1] == "commission":
		var siteID int64
		siteID, err = strconv.ParseInt(os.Args[2], 10, 64)
		if err != nil {
			err = errors.New("site ID must be an integer")
		} else {
			err = commission(ctx, siteID)
		}
	default:
		err = errors.New("usage: camos-gateway.exe [commission <site_id>]")
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}
