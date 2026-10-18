package api

import (
	"encoding/binary"
	"errors"
	"unicode/utf16"
)

// FILE_STREAM_INFO is the SDK's 24-byte fixed header followed by the counted
// UTF-16 name. Only a positively returned ordinary default stream is eligible;
// failed/unsupported queries are not an empty inventory.
func decodeSerenaTreeStreams(raw []byte, limits SerenaTreeLimits) ([]SerenaTreeStream, error) {
	if !limits.valid() || len(raw) < 24 || int64(len(raw)) > limits.EncodedPathBytes {
		return nil, errors.New("SERENA_TREE_STREAM_BUFFER_INVALID")
	}
	var streams []SerenaTreeStream
	var nameBytes int64
	for offset := 0; ; {
		if offset > len(raw)-24 || len(streams) >= limits.Entries {
			return nil, errors.New("SERENA_TREE_STREAM_HEADER_INVALID")
		}
		header := raw[offset:]
		next := uint64(binary.LittleEndian.Uint32(header))
		nameLength := uint64(binary.LittleEndian.Uint32(header[4:]))
		if nameLength == 0 || nameLength%2 != 0 || nameLength > uint64(len(header)-24) || nameLength > uint64(limits.EncodedPathBytes-nameBytes) {
			return nil, errors.New("SERENA_TREE_STREAM_NAME_INVALID")
		}
		size := int64(binary.LittleEndian.Uint64(header[8:]))
		allocated := int64(binary.LittleEndian.Uint64(header[16:]))
		if size < 0 || allocated < 0 || size > limits.LogicalBytes {
			return nil, errors.New("SERENA_TREE_STREAM_SIZE_INVALID")
		}
		units := make([]uint16, int(nameLength/2))
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(header[24+i*2:])
		}
		for i := 0; i < len(units); i++ {
			if units[i] == 0 || units[i] >= 0xdc00 && units[i] <= 0xdfff {
				return nil, errors.New("SERENA_TREE_STREAM_UTF16_INVALID")
			}
			if units[i] >= 0xd800 && units[i] <= 0xdbff {
				if i+1 == len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
					return nil, errors.New("SERENA_TREE_STREAM_UTF16_INVALID")
				}
				i++
			}
		}
		name := string(utf16.Decode(units))
		if name != "::$DATA" || len(streams) != 0 {
			return nil, errors.New("SERENA_TREE_NONDEFAULT_STREAM_REFUSED")
		}
		streams = append(streams, SerenaTreeStream{Name: name, Bytes: size, AllocatedBytes: allocated})
		nameBytes += int64(nameLength)
		if next == 0 {
			return streams, nil
		}
		if next%8 != 0 || next < 24+nameLength || next > uint64(len(header)-24) {
			return nil, errors.New("SERENA_TREE_STREAM_OFFSET_INVALID")
		}
		offset += int(next)
	}
}
