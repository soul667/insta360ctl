package direct

import (
	"context"
	"fmt"

	"github.com/xaionaro-go/insta360ctl/pkg/camera"
	"github.com/xaionaro-go/insta360ctl/pkg/protocol/messagecode"
)

// TakePhoto sends a take-photo command to the camera.
func (d *Device) TakePhoto(ctx context.Context) error {
	_, err := d.SendCommand(ctx, messagecode.CodeTakePhoto, nil)
	if err != nil {
		return fmt.Errorf("take photo failed: %w", err)
	}
	return nil
}

// StartRecording sends a start-recording command to the camera.
func (d *Device) StartRecording(ctx context.Context) error {
	_, err := d.SendCommand(ctx, messagecode.CodeStartRecording, nil)
	if err != nil {
		return fmt.Errorf("start recording failed: %w", err)
	}
	return nil
}

// StopRecording sends a stop-recording command to the camera.
func (d *Device) StopRecording(ctx context.Context) error {
	_, err := d.SendCommand(ctx, messagecode.CodeStopRecording, nil)
	if err != nil {
		return fmt.Errorf("stop recording failed: %w", err)
	}
	return nil
}

// SetMode changes the camera capture mode.
func (d *Device) SetMode(ctx context.Context, mode camera.CaptureMode) error {
	if d.Cam.DirectProtoFormat() == camera.ProtoFormatNetwork {
		// The legacy mode command (0x0C) is officially DELETE_FILES; it is
		// only known to set the mode on the GO 3 firmware. Sending it to an
		// X5 would risk a file operation, so refuse until the right command
		// for network-framed cameras is known.
		return fmt.Errorf("setting the capture mode over BLE is not implemented for network-framed cameras (X5) yet; set the mode on the camera itself")
	}
	payload := []byte{byte(mode)}
	_, err := d.SendCommand(ctx, messagecode.CodeSetCaptureMode, payload)
	if err != nil {
		return fmt.Errorf("set mode failed: %w", err)
	}
	return nil
}

// SetHDR enables or disables HDR mode.
func (d *Device) SetHDR(ctx context.Context, enable bool) error {
	if d.Cam.DirectProtoFormat() == camera.ProtoFormatNetwork {
		// 0x33 is START_HDR_CAPTURE (starts an HDR photo) in the official
		// enum, not an enable/disable switch — do not send it blindly.
		return fmt.Errorf("toggling HDR over BLE is not implemented for network-framed cameras (X5) yet; toggle it on the camera itself")
	}
	var val byte
	if enable {
		val = 1
	}
	_, err := d.SendCommand(ctx, messagecode.CodeSetHDR, []byte{val})
	if err != nil {
		return fmt.Errorf("set HDR failed: %w", err)
	}
	return nil
}

// StartTimelapse starts a timelapse capture.
func (d *Device) StartTimelapse(ctx context.Context) error {
	_, err := d.SendCommand(ctx, messagecode.CodeSetTimelapse, nil)
	if err != nil {
		return fmt.Errorf("start timelapse failed: %w", err)
	}
	return nil
}

// StopTimelapse stops a timelapse capture.
func (d *Device) StopTimelapse(ctx context.Context) error {
	_, err := d.SendCommand(ctx, messagecode.CodeStopTimelapse, nil)
	if err != nil {
		return fmt.Errorf("stop timelapse failed: %w", err)
	}
	return nil
}

// SetHighlight marks the current moment as a highlight in the recording.
func (d *Device) SetHighlight(ctx context.Context) error {
	_, err := d.SendCommand(ctx, messagecode.CodeSetHighlight, nil)
	if err != nil {
		return fmt.Errorf("set highlight failed: %w", err)
	}
	return nil
}

// PowerOff powers off the camera.
func (d *Device) PowerOff(ctx context.Context) error {
	return d.SendCommandNoResponse(ctx, messagecode.CodePowerOff, nil)
}
