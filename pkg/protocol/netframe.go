package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Network framing over BLE.
//
// Newer Insta360 cameras (verified on X5) speak the same packet protocol on
// BLE as on the WiFi TCP control channel (port 6666). Instead of the bare
// 16-byte Header16 header, every BLE notification / write is a length-
// prefixed "network frame":
//
//	[total_len: uint32 LE, includes these 4 bytes]
//	[payload]
//
// where the payload starts with a 1-byte packet type:
//
//	0x04 MESSAGE   — command / response / notification
//	0x05 KEEPALIVE — `05 00 00`, sent by the camera about once a second
//	0x06 SYNC      — `06 00 00` + "syNceNdinS" ASCII magic
//
// MESSAGE payloads use a 12-byte header followed by an optional protobuf
// body:
//
//	Offset  Size  Field
//	0       1     packet type (0x04)
//	1-2     2     reserved (0x00 0x00)
//	3-4     2     message code (uint16 LE)
//	5       1     content type (0x02 = protobuf)
//	6-8     3     sequence number (uint24 LE)
//	9       1     flags (0x80)
//	10-11   2     reserved
//	12+     N     protobuf body
//
// These helpers build and parse that framing. The command codes are the same
// numeric values as the WiFi protocol / messagecode package (0x04
// START_CAPTURE, 0x05 STOP_CAPTURE, ...), and response ACKs arrive as
// MESSAGE packets whose code field is the HTTP-style status (200/400/500).

// Network packet type identifiers (first byte of payload).
const (
	NetPktTypeStream    byte = 0x01
	NetPktTypeMessage   byte = 0x04
	NetPktTypeKeepalive byte = 0x05
	NetPktTypeSync      byte = 0x06
)

// NetSyncMagic is the ASCII magic used in the SYNC handshake.
const NetSyncMagic = "syNceNdinS"

// NetCommandHeaderSize is the fixed MESSAGE header size (without the
// 4-byte length prefix).
const NetCommandHeaderSize = 12

// EncodeNetFrame prepends the 4-byte little-endian total-length prefix.
// The length value includes the 4 prefix bytes themselves.
func EncodeNetFrame(payload []byte) []byte {
	frame := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(frame[0:4], uint32(len(frame)))
	copy(frame[4:], payload)
	return frame
}

// EncodeNetSync returns the complete framed SYNC handshake packet
// (`06 00 00` + "syNceNdinS").
func EncodeNetSync() []byte {
	payload := make([]byte, 0, 3+len(NetSyncMagic))
	payload = append(payload, NetPktTypeSync, 0x00, 0x00)
	payload = append(payload, NetSyncMagic...)
	return EncodeNetFrame(payload)
}

// EncodeNetKeepalive returns the complete framed KEEPALIVE packet.
func EncodeNetKeepalive() []byte {
	return EncodeNetFrame([]byte{NetPktTypeKeepalive, 0x00, 0x00})
}

// EncodeNetMessage builds a complete framed MESSAGE packet: the 12-byte
// network header, the optional body, and the 4-byte length prefix.
func EncodeNetMessage(cmd uint16, seq uint32, body []byte) []byte {
	pkt := make([]byte, NetCommandHeaderSize+len(body))
	pkt[0] = NetPktTypeMessage
	pkt[1] = 0x00
	pkt[2] = 0x00
	binary.LittleEndian.PutUint16(pkt[3:5], cmd)
	pkt[5] = 0x02 // content type: protobuf
	pkt[6] = byte(seq)
	pkt[7] = byte(seq >> 8)
	pkt[8] = byte(seq >> 16)
	pkt[9] = 0x80
	pkt[10] = 0x00
	pkt[11] = 0x00
	copy(pkt[NetCommandHeaderSize:], body)
	return EncodeNetFrame(pkt)
}

// DecodeNetFrame splits an incoming BLE notification into the payload
// following the length prefix. It returns io.ErrUnexpectedEOF when the
// frame is incomplete (more BLE notifications are needed) and reports the
// expected total size in that case.
func DecodeNetFrame(b []byte) (payload []byte, needed int, err error) {
	if len(b) < 4 {
		return nil, 4, fmt.Errorf("%w: need 4 bytes for the length prefix, got %d", io.ErrUnexpectedEOF, len(b))
	}
	total := binary.LittleEndian.Uint32(b[0:4])
	if total < 4 {
		return nil, 0, fmt.Errorf("invalid network frame length: %d", total)
	}
	if int(total) > len(b) {
		return nil, int(total), fmt.Errorf("%w: frame declares %d bytes, got %d", io.ErrUnexpectedEOF, total, len(b))
	}
	return b[4:total], 0, nil
}

// NetMessage is a parsed network MESSAGE packet.
type NetMessage struct {
	Code uint16 // command, response status (200/400/500/501) or notification code
	Seq  uint32 // uint24 sequence number
	Body []byte // protobuf body (may be empty)
}

// ParseNetMessage parses a network MESSAGE payload (the bytes after the
// length prefix, i.e. starting with the 0x04 packet type byte).
func ParseNetMessage(payload []byte) (*NetMessage, error) {
	if len(payload) < NetCommandHeaderSize {
		return nil, fmt.Errorf("%w: need %d bytes for the network message header, got %d",
			io.ErrUnexpectedEOF, NetCommandHeaderSize, len(payload))
	}
	if payload[0] != NetPktTypeMessage {
		return nil, fmt.Errorf("not a network MESSAGE packet: type=0x%02X", payload[0])
	}
	msg := &NetMessage{
		Code: binary.LittleEndian.Uint16(payload[3:5]),
		Seq:  uint32(payload[6]) | uint32(payload[7])<<8 | uint32(payload[8])<<16,
	}
	if len(payload) > NetCommandHeaderSize {
		msg.Body = payload[NetCommandHeaderSize:]
	}
	return msg, nil
}
