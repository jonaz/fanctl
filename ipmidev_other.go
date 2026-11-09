//go:build !(linux && (amd64 || arm64))

package main

import (
	"context"
	"errors"
)

// ipmiDevice is only implemented for 64-bit Linux; elsewhere use ipmitool.
type ipmiDevice struct{}

var errNoIPMIDevice = errors.New(`the IPMI device backend needs linux/amd64 or linux/arm64; set "ipmi": "ipmitool" in the config`)

func openIPMIDevice(string) (*ipmiDevice, error) { return nil, errNoIPMIDevice }

func (*ipmiDevice) name() string { return "ipmi device" }
func (*ipmiDevice) Close() error { return nil }
func (*ipmiDevice) raw(context.Context, byte, byte, []byte) error {
	return errNoIPMIDevice
}
