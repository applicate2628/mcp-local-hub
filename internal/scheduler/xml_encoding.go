package scheduler

import "bytes"

// EncodeXMLUTF16LEBOM converts a UTF-8 XML string to UTF-16 LE with a BOM,
// the byte format required by Task Scheduler's /XML flag.
func EncodeXMLUTF16LEBOM(s string) []byte {
	var out bytes.Buffer
	out.WriteByte(0xFF)
	out.WriteByte(0xFE) // UTF-16 LE BOM
	for _, r := range s {
		if r <= 0xFFFF {
			out.WriteByte(byte(r))
			out.WriteByte(byte(r >> 8))
			continue
		}
		r -= 0x10000
		hi := 0xD800 + (r >> 10)
		lo := 0xDC00 + (r & 0x3FF)
		out.WriteByte(byte(hi))
		out.WriteByte(byte(hi >> 8))
		out.WriteByte(byte(lo))
		out.WriteByte(byte(lo >> 8))
	}
	return out.Bytes()
}
