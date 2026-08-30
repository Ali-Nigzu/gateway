package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"

	connect "github.com/Ali-Nigzu/gateway/connect"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) != 1 {
		return fail(errors.New("usage: commission <site_id>"))
	}

	siteID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || siteID <= 0 {
		return fail(errors.New("site ID must be a positive integer"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Printf("Commissioning site %d\n", siteID)
	if err := connect.Commission(ctx, siteID); err != nil {
		return fail(err)
	}
	fmt.Println("PASS")
	return 0
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
	return 1
}
