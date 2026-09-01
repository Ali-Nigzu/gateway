package main

import "context"

func commission(ctx context.Context, siteID int64) error {
	if err := saveSiteID(siteID); err != nil {
		return err
	}
	return startGateway(ctx, siteID)
}
