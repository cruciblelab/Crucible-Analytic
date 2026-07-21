package ja4

import "encoding/binary"

// cursor is a minimal forward-only reader over a byte slice, used to walk
// TLS ClientHello wire structures without pulling in a full parsing
// dependency.
type cursor struct {
	b   []byte
	pos int
}

func (c *cursor) remaining() int { return len(c.b) - c.pos }

func (c *cursor) readUint8() (uint8, bool) {
	if c.remaining() < 1 {
		return 0, false
	}
	v := c.b[c.pos]
	c.pos++
	return v, true
}

func (c *cursor) readUint16() (uint16, bool) {
	if c.remaining() < 2 {
		return 0, false
	}
	v := binary.BigEndian.Uint16(c.b[c.pos:])
	c.pos += 2
	return v, true
}

func (c *cursor) readUint24() (uint32, bool) {
	if c.remaining() < 3 {
		return 0, false
	}
	v := uint32(c.b[c.pos])<<16 | uint32(c.b[c.pos+1])<<8 | uint32(c.b[c.pos+2])
	c.pos += 3
	return v, true
}

func (c *cursor) skip(n int) bool {
	if n < 0 || c.remaining() < n {
		return false
	}
	c.pos += n
	return true
}

func (c *cursor) readBytes(n int) ([]byte, bool) {
	if n < 0 || c.remaining() < n {
		return nil, false
	}
	v := c.b[c.pos : c.pos+n]
	c.pos += n
	return v, true
}
