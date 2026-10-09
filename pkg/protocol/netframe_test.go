package protocol

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// The golden values in this file were captured from a real Insta360 X5 over
// BLE (firmware as of 2026-10) during reverse-engineering: the camera sends
// these frames, and the commands below are the ones it accepted with HTTP-200
// acknowledgments.

func TestEncodeNetKeepaliveMatchesCamera(t *testing.T) {
	// The camera sends this frame ~1/sec on BE82.
	require.Equal(t, []byte{0x07, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00}, EncodeNetKeepalive())
}

func TestEncodeNetSyncMatchesHandshake(t *testing.T) {
	require.Equal(t,
		[]byte{0x11, 0x00, 0x00, 0x00, 0x06, 0x00, 0x00, 0x73, 0x79, 0x4E, 0x63, 0x65, 0x4E, 0x64, 0x69, 0x6E, 0x53},
		EncodeNetSync())
}

func TestEncodeNetMessageStartCapture(t *testing.T) {
	// START_CAPTURE (0x04), seq 1, no body — accepted by a real X5.
	require.Equal(t,
		[]byte{0x10, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x04, 0x00, 0x02, 0x01, 0x00, 0x00, 0x80, 0x00, 0x00},
		EncodeNetMessage(0x04, 1, nil))
}

func TestDecodeNetFrameKeepalive(t *testing.T) {
	payload, needed, err := DecodeNetFrame([]byte{0x07, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00})
	require.NoError(t, err)
	require.Equal(t, 0, needed)
	require.Equal(t, []byte{0x05, 0x00, 0x00}, payload)
}

func TestDecodeNetFrameIncomplete(t *testing.T) {
	_, needed, err := DecodeNetFrame([]byte{0x10, 0x00, 0x00, 0x00, 0x04})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, 16, needed)
}

func TestDecodeNetFrameRejectsShortPrefix(t *testing.T) {
	_, needed, err := DecodeNetFrame([]byte{0x10, 0x00})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, 4, needed)
}

func TestParseNetMessageACK(t *testing.T) {
	// Captured: ACK of START_CAPTURE, echoing seq 1 with code 200.
	frame := []byte{0x10, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0xC8, 0x00, 0x02, 0x01, 0x00, 0x00, 0x80, 0x00, 0x00}
	payload, _, err := DecodeNetFrame(frame)
	require.NoError(t, err)

	msg, err := ParseNetMessage(payload)
	require.NoError(t, err)
	require.Equal(t, uint16(0x00C8), msg.Code)
	require.Equal(t, uint32(1), msg.Seq)
	require.Empty(t, msg.Body)
}

func TestParseNetMessageStopACKCarriesFilename(t *testing.T) {
	// Captured: ACK of STOP_CAPTURE with the recorded file path in the body.
	frame := []byte{
		0x42, 0x00, 0x00, 0x00,
		0x04, 0x00, 0x00, 0xC8, 0x00, 0x02, 0x03, 0x00, 0x00, 0x80, 0x00, 0x00,
		0x0A, 0x30, 0x0A, 0x2E, 0x2F, 0x44, 0x43, 0x49, 0x4D, 0x2F, 0x43, 0x61, 0x6D, 0x65, 0x72, 0x61,
		0x30, 0x31, 0x2F, 0x56, 0x49, 0x44, 0x5F, 0x32, 0x30, 0x32, 0x36, 0x31, 0x30, 0x30, 0x39, 0x5F,
		0x31, 0x34, 0x34, 0x31, 0x33, 0x31, 0x5F, 0x30, 0x30, 0x5F, 0x30, 0x35, 0x34, 0x2E, 0x69, 0x6E,
		0x73, 0x76,
	}
	payload, _, err := DecodeNetFrame(frame)
	require.NoError(t, err)

	msg, err := ParseNetMessage(payload)
	require.NoError(t, err)
	require.Equal(t, uint16(0x00C8), msg.Code)
	require.Equal(t, uint32(3), msg.Seq)
	require.Contains(t, string(msg.Body), "VID_20261009_144131_00_054.insv")
}

func TestParseNetMessageRejectsOtherTypes(t *testing.T) {
	_, err := ParseNetMessage([]byte{0x05, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a network MESSAGE")
}

func TestEncodeNetMessageRoundTrip(t *testing.T) {
	body := []byte{0x08, 0x01}
	frame := EncodeNetMessage(0x10, 42, body)

	payload, _, err := DecodeNetFrame(frame)
	require.NoError(t, err)

	msg, err := ParseNetMessage(payload)
	require.NoError(t, err)
	require.Equal(t, uint16(0x10), msg.Code)
	require.Equal(t, uint32(42), msg.Seq)
	require.Equal(t, body, msg.Body)
}
