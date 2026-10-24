package collector

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
)

// lz4 frame format constants (https://github.com/lz4/lz4/blob/dev/doc/lz4_Frame_format.md).
const (
	lz4FrameMagic     = 0x184d2204
	lz4SkippableMagic = 0x184d2a50 // to 0x184d2a5f
	lz4Window         = 64 << 10
	lz4MaximumBlock   = 4 << 20
)

var errLZ4 = errors.New("malformed lz4 data")

// lz4Reader decompresses lz4 frames, the format apt stores *.lz4 lists in,
// as a stream: it keeps the last 64 KiB of output for back references of
// linked blocks and one block of input. Checksums are not verified.
type lz4Reader struct {
	input *bufio.Reader
	// history ends with the output of the current block; pending is the
	// part of it not yet read.
	history []byte
	pending []byte
	block   []byte
	// inFrame is set between a frame header and its end mark.
	inFrame          bool
	blockChecksum    bool
	contentChecksum  bool
	independentBlock bool
	err              error
}

func newLZ4Reader(reader io.Reader) *lz4Reader {
	return &lz4Reader{input: bufio.NewReaderSize(reader, 64<<10)}
}

func (reader *lz4Reader) Read(buffer []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.err != nil {
			return 0, reader.err
		}
		reader.err = reader.next()
	}
	count := copy(buffer, reader.pending)
	reader.pending = reader.pending[count:]
	return count, nil
}

// next decodes the next block, reading frame headers and end marks on the
// way. It returns io.EOF after the last frame.
func (reader *lz4Reader) next() error {
	if !reader.inFrame {
		if err := reader.frameHeader(); err != nil {
			return err
		}
	}
	var size uint32
	if err := binary.Read(reader.input, binary.LittleEndian, &size); err != nil {
		return unexpected(err)
	}
	if size == 0 { // end mark
		reader.inFrame = false
		if reader.contentChecksum {
			if _, err := reader.input.Discard(4); err != nil {
				return unexpected(err)
			}
		}
		return nil
	}
	uncompressed := size&0x80000000 != 0
	size &= 0x7fffffff
	if size > lz4MaximumBlock {
		return errLZ4
	}
	if cap(reader.block) < int(size) {
		reader.block = make([]byte, size)
	}
	block := reader.block[:size]
	if _, err := io.ReadFull(reader.input, block); err != nil {
		return unexpected(err)
	}
	if reader.blockChecksum {
		if _, err := reader.input.Discard(4); err != nil {
			return unexpected(err)
		}
	}

	// Keep at most a window of history before the new block.
	if len(reader.history) > lz4Window {
		reader.history = append(reader.history[:0], reader.history[len(reader.history)-lz4Window:]...)
	}
	if reader.independentBlock {
		reader.history = reader.history[:0]
	}
	start := len(reader.history)
	if uncompressed {
		reader.history = append(reader.history, block...)
	} else {
		var err error
		if reader.history, err = lz4DecodeBlock(reader.history, block); err != nil {
			return err
		}
	}
	reader.pending = reader.history[start:]
	return nil
}

// frameHeader reads a frame descriptor, skipping skippable frames. A clean
// end of input before a frame is io.EOF.
func (reader *lz4Reader) frameHeader() error {
	for {
		var magic uint32
		if err := binary.Read(reader.input, binary.LittleEndian, &magic); err != nil {
			if errors.Is(err, io.EOF) {
				return io.EOF
			}
			return unexpected(err)
		}
		if magic&0xfffffff0 == lz4SkippableMagic {
			var size uint32
			if err := binary.Read(reader.input, binary.LittleEndian, &size); err != nil {
				return unexpected(err)
			}
			if _, err := reader.input.Discard(int(size)); err != nil {
				return unexpected(err)
			}
			continue
		}
		if magic != lz4FrameMagic {
			return errLZ4
		}
		var descriptor [2]byte
		if _, err := io.ReadFull(reader.input, descriptor[:]); err != nil {
			return unexpected(err)
		}
		flags := descriptor[0]
		if flags>>6 != 1 || flags&0x02 != 0 {
			return errLZ4
		}
		reader.independentBlock = flags&0x20 != 0
		reader.blockChecksum = flags&0x10 != 0
		reader.contentChecksum = flags&0x04 != 0
		skip := 1 // header checksum
		if flags&0x08 != 0 {
			skip += 8 // content size
		}
		if flags&0x01 != 0 {
			skip += 4 // dictionary id
		}
		if _, err := reader.input.Discard(skip); err != nil {
			return unexpected(err)
		}
		reader.inFrame = true
		reader.history = reader.history[:0]
		return nil
	}
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// lz4DecodeBlock appends the decompressed block to output, whose existing
// content is the history that matches may refer back to.
func lz4DecodeBlock(output, block []byte) ([]byte, error) {
	position := 0
	length := func(initial int) (int, bool) {
		value := initial
		if initial != 15 {
			return value, true
		}
		for {
			if position >= len(block) {
				return 0, false
			}
			next := int(block[position])
			position++
			value += next
			if value > lz4MaximumBlock*2 {
				return 0, false
			}
			if next != 255 {
				return value, true
			}
		}
	}
	for position < len(block) {
		token := block[position]
		position++
		literals, ok := length(int(token >> 4))
		if !ok || position+literals > len(block) {
			return output, errLZ4
		}
		output = append(output, block[position:position+literals]...)
		position += literals
		if position == len(block) {
			return output, nil // the last sequence has no match
		}
		if position+2 > len(block) {
			return output, errLZ4
		}
		offset := int(block[position]) | int(block[position+1])<<8
		position += 2
		match, ok := length(int(token & 0x0f))
		if !ok || offset == 0 || offset > len(output) {
			return output, errLZ4
		}
		match += 4
		from := len(output) - offset
		if offset >= match {
			output = append(output, output[from:from+match]...)
			continue
		}
		for index := range match { // overlapping: a repeated pattern
			output = append(output, output[from+index])
		}
	}
	return output, nil
}
