// Copyright 2023 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package common

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/google/uuid"
	"github.com/gravitational/trace"
	"github.com/gravitational/trace/trail"
	"google.golang.org/protobuf/types/known/timestamppb"

	devicepb "github.com/gravitational/teleport/api/gen/proto/go/teleport/devicetrust/v1"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/asciitable"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/devicetrust"
	"github.com/gravitational/teleport/lib/service/servicecfg"
)

// DevicesCommand implements the `tctl devices` command.
// Device trust is an enterprise-only feature, so this
// command will fail when run against an OSS auth server.
type DevicesCommand struct {
	add    deviceAddCommand
	ls     deviceListCommand
	rm     deviceRemoveCommand
	enroll deviceEnrollCommand
	lock   deviceLockCommand
}

type osType = string

const (
	linuxType   osType = "linux"
	macosType   osType = "macos"
	windowsType osType = "windows"
)

var osTypes = []string{linuxType, macosType, windowsType}

var osTypeToEnum = map[osType]devicepb.OSType{
	linuxType:   devicepb.OSType_OS_TYPE_LINUX,
	macosType:   devicepb.OSType_OS_TYPE_MACOS,
	windowsType: devicepb.OSType_OS_TYPE_WINDOWS,
}

func (c *DevicesCommand) Initialize(app *kingpin.Application, cfg *servicecfg.Config) {
	devicesCmd := app.Command("devices", "Register and manage trusted devices").Hidden()

	addCmd := devicesCmd.Command("add", "Register managed devices.")
	addCmd.Flag("os", "Operating system").
		Required().
		EnumVar(&c.add.os, osTypes...)
	addCmd.Flag("asset-tag", "Inventory identifier for the device (e.g., Mac serial number)").
		Required().
		StringVar(&c.add.assetTag)
	addCmd.Flag("enroll", "If set, creates a device enrollment token").
		BoolVar(&c.add.enroll)
	addCmd.Flag("enroll-ttl", "Time duration for the enrollment token").
		DurationVar(&c.add.enrollTTL)

	_ = devicesCmd.Command("ls", "Lists managed devices.")

	rmCmd := devicesCmd.Command("rm", "Removes a managed device.")
	rmCmd.Flag("device-id", "Device identifier").StringVar(&c.rm.deviceID)
	rmCmd.Flag("asset-tag", "Inventory identifier for the device").StringVar(&c.rm.assetTag)

	enrollCmd := devicesCmd.Command("enroll", "Creates a new device enrollment token.")
	enrollCmd.Flag("device-id", "Device identifier").StringVar(&c.enroll.deviceID)
	enrollCmd.Flag("asset-tag", "Inventory identifier for the device").StringVar(&c.enroll.assetTag)
	enrollCmd.Flag("ttl", "Time duration for the enrollment token").DurationVar(&c.enroll.ttl)

	lockCmd := devicesCmd.Command("lock", "Locks a device.")
	lockCmd.Flag("device-id", "Device identifier").StringVar(&c.lock.deviceID)
	lockCmd.Flag("asset-tag", "Inventory identifier for the device").StringVar(&c.lock.assetTag)
	lockCmd.Flag("message", "Message to display to locked-out users").StringVar(&c.lock.message)
	lockCmd.Flag("expires", "Time point (RFC3339) when the lock expires").StringVar(&c.lock.expires)
	lockCmd.Flag("ttl", "Time duration after which the lock expires").DurationVar(&c.lock.ttl)
}

// runner is used as a simple interface for subcommands.
type runner interface {
	Run(context.Context, auth.ClientI) error
}

func (c *DevicesCommand) TryRun(ctx context.Context, selectedCommand string, authClient auth.ClientI) (match bool, err error) {
	innerCmd, ok := map[string]runner{
		"devices add":    &c.add,
		"devices ls":     &c.ls,
		"devices rm":     &c.rm,
		"devices enroll": &c.enroll,
		"devices lock":   &c.lock,
	}[selectedCommand]
	if !ok {
		return false, nil
	}

	switch err := trail.FromGRPC(innerCmd.Run(ctx, authClient)); {
	case trace.IsNotImplemented(err):
		return true, trace.AccessDenied("Device Trust requires a Teleport Enterprise Auth Server running v12 or later.")
	default:
		return true, trace.Wrap(err)
	}
}

type deviceAddCommand struct {
	os        string
	assetTag  string
	enroll    bool
	enrollTTL time.Duration
}

func (c *deviceAddCommand) Run(ctx context.Context, authClient auth.ClientI) error {
	osType, ok := osTypeToEnum[c.os]
	if !ok {
		return trace.BadParameter("invalid --os: %v", c.os)
	}

	var enrollExpireTime *timestamppb.Timestamp
	if c.enrollTTL > 0 {
		enrollExpireTime = timestamppb.New(time.Now().Add(c.enrollTTL))
	}
	created, err := authClient.DevicesClient().CreateDevice(ctx, &devicepb.CreateDeviceRequest{
		Device: &devicepb.Device{
			OsType:   osType,
			AssetTag: c.assetTag,
		},
		CreateEnrollToken:     c.enroll,
		EnrollTokenExpireTime: enrollExpireTime,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	fmt.Printf(
		"Device %v/%v added to the inventory\n",
		created.AssetTag,
		devicetrust.FriendlyOSType(created.OsType))
	printEnrollMessage(created.AssetTag, created.EnrollToken)

	return nil
}

func printEnrollMessage(name string, token *devicepb.DeviceEnrollToken) {
	if token.GetToken() == "" {
		return
	}
	expireTime := token.ExpireTime.AsTime()

	fmt.Printf(`The enrollment token: %v
This token will expire in %v.

Run the command below on device %q to enroll it:
tsh device enroll --token=%v
`,
		token.Token, time.Until(expireTime).Round(time.Second), name, token.Token,
	)
}

type deviceListCommand struct{}

func (c *deviceListCommand) Run(ctx context.Context, authClient auth.ClientI) error {
	devices := authClient.DevicesClient()

	// List all devices.
	req := &devicepb.ListDevicesRequest{
		View: devicepb.DeviceView_DEVICE_VIEW_LIST,
	}
	var devs []*devicepb.Device
	for {
		resp, err := devices.ListDevices(ctx, req)
		if err != nil {
			return trace.Wrap(err)
		}

		devs = append(devs, resp.Devices...)

		if resp.NextPageToken == "" {
			break
		}
		req.PageToken = resp.NextPageToken
	}
	if len(devs) == 0 {
		fmt.Println("No devices found")
		return nil
	}

	// Sort by {AssetTag, OsType}.
	sort.Slice(devs, func(i, j int) bool {
		d1 := devs[i]
		d2 := devs[j]

		if d1.AssetTag == d2.AssetTag {
			return d1.OsType < d2.OsType
		}

		return d1.AssetTag < d2.AssetTag
	})

	// Print devices.
	table := asciitable.MakeTable([]string{"Asset Tag", "OS", "Enroll Status", "Device ID"})
	for _, dev := range devs {
		table.AddRow([]string{
			dev.AssetTag,
			devicetrust.FriendlyOSType(dev.OsType),
			devicetrust.FriendlyDeviceEnrollStatus(dev.EnrollStatus),
			dev.Id,
		})
	}
	fmt.Println(table.AsBuffer().String())

	return nil
}

type deviceRemoveCommand struct {
	deviceID, assetTag string
}

func (c *deviceRemoveCommand) Run(ctx context.Context, authClient auth.ClientI) error {
	switch {
	case c.deviceID == "" && c.assetTag == "":
		return trace.BadParameter("either --device-id or --asset-tag must be set")
	case c.deviceID != "" && c.assetTag != "":
		return trace.BadParameter("only one of --device-id or --asset-tag must be set")
	}

	devices := authClient.DevicesClient()

	// Find the specified device, if necessary.
	deviceID, name, err := findDeviceID(ctx, devices, c.deviceID, c.assetTag)
	if err != nil {
		return trace.Wrap(err)
	}

	if _, err := devices.DeleteDevice(ctx, &devicepb.DeleteDeviceRequest{
		DeviceId: deviceID,
	}); err != nil {
		return trace.Wrap(err)
	}

	fmt.Printf("Device %q removed\n", name)
	return nil
}

type deviceEnrollCommand struct {
	deviceID, assetTag string
	ttl                time.Duration
}

func (c *deviceEnrollCommand) Run(ctx context.Context, authClient auth.ClientI) error {
	switch {
	case c.deviceID == "" && c.assetTag == "":
		return trace.BadParameter("either --device-id or --asset-tag must be set")
	case c.deviceID != "" && c.assetTag != "":
		return trace.BadParameter("only one of --device-id or --asset-tag must be set")
	}

	devices := authClient.DevicesClient()

	// Find the specified device, if necessary.
	deviceID, name, err := findDeviceID(ctx, devices, c.deviceID, c.assetTag)
	if err != nil {
		return trace.Wrap(err)
	}

	var expireTime *timestamppb.Timestamp
	if c.ttl > 0 {
		expireTime = timestamppb.New(time.Now().Add(c.ttl))
	}
	token, err := devices.CreateDeviceEnrollToken(ctx, &devicepb.CreateDeviceEnrollTokenRequest{
		DeviceId:   deviceID,
		ExpireTime: expireTime,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	printEnrollMessage(name, token)
	return nil
}

type deviceLockCommand struct {
	deviceID, assetTag string
	message            string
	expires            string
	ttl                time.Duration
}

func (c *deviceLockCommand) Run(ctx context.Context, authClient auth.ClientI) error {
	switch {
	case c.deviceID == "" && c.assetTag == "":
		return trace.BadParameter("either --device-id or --asset-tag must be set")
	case c.deviceID != "" && c.assetTag != "":
		return trace.BadParameter("only one of --device-id or --asset-tag must be set")
	case c.expires != "" && c.ttl != 0:
		return trace.BadParameter("use only one of --expires and --ttl")
	}

	var expires *time.Time
	switch {
	case c.expires != "":
		t, err := time.Parse(time.RFC3339, c.expires)
		if err != nil {
			return trace.Wrap(err)
		}
		expires = &t
	case c.ttl != 0:
		t := time.Now().UTC().Add(c.ttl)
		expires = &t
	}

	deviceID, _, err := findDeviceID(ctx, authClient.DevicesClient(), c.deviceID, c.assetTag)
	if err != nil {
		return trace.Wrap(err)
	}

	lock, err := types.NewLock(uuid.NewString(), types.LockSpecV2{
		Target: types.LockTarget{
			Device: deviceID,
		},
		Message: c.message,
		Expires: expires,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	if err := authClient.UpsertLock(ctx, lock); err != nil {
		return trace.Wrap(err)
	}

	fmt.Printf("Created a lock with name %q.\n", lock.GetName())
	return nil
}

// findDeviceID finds the device ID when supplied with either a deviceID or
// assetTag. If supplied with the former, no backend queries are made. It exists
// to simplify the logic of commands that take either --device-id or --asset-tag
// as an argument.
// Returns the device ID and a name that can be used for CLI messages, the
// latter matching whatever was originally supplied - the device ID or the asset
// tag.
func findDeviceID(ctx context.Context, devices devicepb.DeviceTrustServiceClient, deviceID, assetTag string) (id, name string, err error) {
	if deviceID != "" {
		// No need to query.
		return deviceID, deviceID, nil
	}

	resp, err := devices.FindDevices(ctx, &devicepb.FindDevicesRequest{
		IdOrTag: assetTag,
	})
	if err != nil {
		return "", "", trace.Wrap(err)
	}
	for _, found := range resp.Devices {
		// Skip ID matches.
		if found.AssetTag != assetTag {
			continue
		}

		// Sanity check.
		if deviceID != "" {
			return "", "", trace.BadParameter(
				"found multiple devices for asset tag %q, please retry using the device ID instead", assetTag)
		}

		deviceID = found.Id
	}
	if deviceID == "" {
		return "", "", trace.NotFound("device %q not found", assetTag)
	}

	return deviceID, assetTag, nil
}
