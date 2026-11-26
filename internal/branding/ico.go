package branding

import "encoding/binary"

// PNGIcon describes an already encoded PNG for the ICO directory.
type PNGIcon struct {
	Width, Height int
	PNG           []byte
}

// PackPNGICO writes the provided, internally rendered PNGs into an ICO file.
// Dimensions are 1 through 256 pixels. The directory stores 256 as zero.
func PackPNGICO(images ...PNGIcon) []byte {
	const directorySize, entrySize = 6, 16
	offset := directorySize + entrySize*len(images)
	size := offset
	for _, image := range images {
		size += len(image.PNG)
	}
	data := make([]byte, size)
	order := binary.LittleEndian
	order.PutUint16(data[2:4], 1) // icon, not cursor
	order.PutUint16(data[4:6], uint16(len(images)))
	for i, image := range images {
		start := directorySize + i*entrySize
		entry := data[start : start+entrySize]
		entry[0], entry[1] = byte(image.Width), byte(image.Height)
		order.PutUint16(entry[4:6], 1)  // planes
		order.PutUint16(entry[6:8], 32) // bits per pixel
		order.PutUint32(entry[8:12], uint32(len(image.PNG)))
		order.PutUint32(entry[12:16], uint32(offset))
		offset += copy(data[offset:], image.PNG)
	}
	return data
}
