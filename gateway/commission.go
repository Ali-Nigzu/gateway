package main

import (
	"errors"

	"github.com/google/uuid"
)

func commission(value string) error {
	gatewayID, err := uuid.Parse(value)
	if err != nil {
		return errors.New("gateway ID must be a UUID")
	}
	return installService(gatewayID)
}
