package hubsync

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

// WriteLengthPrefixed writes a length-prefixed protobuf message to w.
// Format: [4 bytes big-endian length][protobuf bytes]
func WriteLengthPrefixed(w io.Writer, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal proto: %w", err)
	}
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(len(data)))
	if _, err := w.Write(buf[:]); err != nil {
		return fmt.Errorf("write length prefix: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write proto data: %w", err)
	}
	return nil
}

// ReadLengthPrefixed reads one length-prefixed protobuf message from r.
func ReadLengthPrefixed(r io.Reader, msg proto.Message) error {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return err // pass through io.EOF / io.ErrUnexpectedEOF
	}
	size := binary.BigEndian.Uint32(buf[:])
	if size > 64*1024*1024 { // 64MB sanity limit
		return fmt.Errorf("message too large: %d bytes", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return fmt.Errorf("read proto data: %w", err)
	}
	return proto.Unmarshal(data, msg)
}
