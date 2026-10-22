package quickfix

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
)

const boundedHeartbeatFrame = "8=FIX.4.4\x019=5\x0135=0\x0110=163\x01"

type boundedFramingReader struct {
	remaining   []byte
	chunk       int
	eofWithData bool
	reads       int
}

func (reader *boundedFramingReader) Read(dst []byte) (int, error) {
	reader.reads++
	if len(reader.remaining) == 0 {
		return 0, io.EOF
	}
	if reader.chunk > 0 && len(dst) > reader.chunk {
		dst = dst[:reader.chunk]
	}
	count := copy(dst, reader.remaining)
	reader.remaining = reader.remaining[count:]
	if len(reader.remaining) == 0 && reader.eofWithData {
		return count, io.EOF
	}
	return count, nil
}

func TestBoundedParserFragmentedAndCoalescedFrames(t *testing.T) {
	const second = "8=FIX.4.4\x019=5\x0135=1\x0110=164\x01"
	for _, chunk := range []int{1, 2, 7, 0} {
		for _, capacity := range []int{len(boundedHeartbeatFrame), 2 * len(boundedHeartbeatFrame)} {
			reader := &boundedFramingReader{remaining: []byte("noise" + boundedHeartbeatFrame + second), chunk: chunk, eofWithData: true}
			framer, err := newBoundedParser(reader, capacity)
			if err != nil {
				t.Fatal(err)
			}
			scratch := &framer.bigBuffer[0]
			firstStorage := make([]byte, len(boundedHeartbeatFrame))
			first, err := framer.ReadMessageInto(firstStorage[:3])
			if err != nil || string(first) != boundedHeartbeatFrame || &first[0] != &firstStorage[0] {
				t.Fatalf("chunk=%d capacity=%d first=%q err=%v", chunk, capacity, first, err)
			}
			secondStorage := make([]byte, len(second))
			next, err := framer.ReadMessageInto(secondStorage)
			if err != nil || string(next) != second || string(first) != boundedHeartbeatFrame {
				t.Fatalf("chunk=%d capacity=%d next=%q err=%v", chunk, capacity, next, err)
			}
			if len(framer.bigBuffer) != capacity || &framer.bigBuffer[0] != scratch {
				t.Fatal("framing scratch changed after reuse")
			}
			reads := reader.reads
			if _, err := framer.ReadMessageInto(secondStorage); err != io.EOF {
				t.Fatalf("after final frame error=%v, want EOF", err)
			}
			if reader.reads != reads {
				t.Fatal("parser discarded EOF returned together with the final bytes")
			}
		}
	}
}

func TestBoundedParserShortDestinationRetainsFrame(t *testing.T) {
	for _, chunk := range []int{1, 0} {
		reader := &boundedFramingReader{remaining: []byte(boundedHeartbeatFrame + boundedHeartbeatFrame), chunk: chunk}
		framer, err := newBoundedParser(reader, 2*len(boundedHeartbeatFrame))
		if err != nil {
			t.Fatal(err)
		}
		short := bytes.Repeat([]byte{0xa5}, len(boundedHeartbeatFrame)-1)
		before := bytes.Clone(short)
		result, err := framer.ReadMessageInto(short[:1])
		if err != ErrFrameCapacity || len(result) != 0 || cap(result) != cap(short) || !bytes.Equal(short, before) {
			t.Fatalf("short destination changed or wrong error: %v", err)
		}
		if result, err := framer.ReadMessageInto(nil); err != ErrFrameCapacity || result != nil {
			t.Fatalf("nil destination returned %v, %v", result, err)
		}
		storage := make([]byte, len(boundedHeartbeatFrame))
		for index := 0; index < 2; index++ {
			frame, err := framer.ReadMessageInto(storage)
			if err != nil || string(frame) != boundedHeartbeatFrame {
				t.Fatalf("retry frame %d = %q, %v", index, frame, err)
			}
		}
	}
}

func TestBoundedParserCapacityAndMalformedLength(t *testing.T) {
	cases := []struct {
		name     string
		wire     string
		capacity int
		expected error
	}{
		{"frame over capacity", boundedHeartbeatFrame, len(boundedHeartbeatFrame) - 1, ErrFrameCapacity},
		{"scratch full before start", strings.Repeat("x", 32), 32, ErrFrameCapacity},
		{"missing length value", "8=FIX.4.4\x019=\x01", 128, errInvalidFrameLength},
		{"zero length", "8=FIX.4.4\x019=0\x01", 128, errInvalidFrameLength},
		{"negative length", "8=FIX.4.4\x019=-1\x01", 128, errInvalidFrameLength},
		{"signed length", "8=FIX.4.4\x019=+5\x01", 128, errInvalidFrameLength},
		{"wrong second field", "8=FIX.4.4\x0135=0\x01", 128, errInvalidFrameLength},
		{"overflow length", "8=FIX.4.4\x019=999999999999999999999999999999\x01", 128, ErrFrameCapacity},
		{"length too short", "8=FIX.4.4\x019=4\x0135=0\x0110=163\x01" + boundedHeartbeatFrame, 128, errInvalidFrameChecksum},
		{"length too long", "8=FIX.4.4\x019=6\x0135=0\x0110=163\x01" + boundedHeartbeatFrame, 128, errInvalidFrameChecksum},
		{"checksum not numeric", "8=FIX.4.4\x019=5\x0135=0\x0110=a63\x01", 128, errInvalidFrameChecksum},
		{"checksum wrong width", "8=FIX.4.4\x019=5\x0135=0\x0110=0163\x01", 128, errInvalidFrameChecksum},
	}
	for _, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			framer, err := newBoundedParser(strings.NewReader(fixture.wire), fixture.capacity)
			if err != nil {
				t.Fatal(err)
			}
			storage := bytes.Repeat([]byte{0xa5}, 256)
			before := bytes.Clone(storage)
			frame, err := framer.ReadMessageInto(storage)
			if err != fixture.expected || len(frame) != 0 || !bytes.Equal(storage, before) {
				t.Fatalf("frame=%q error=%v, want %v without output mutation", frame, err, fixture.expected)
			}
			if _, retryErr := framer.ReadMessageInto(storage); retryErr != fixture.expected {
				t.Fatalf("retry error=%v, want original error %v", retryErr, fixture.expected)
			}
		})
	}
}

func TestBoundedParserPartialEOF(t *testing.T) {
	for length := 0; length < len(boundedHeartbeatFrame); length++ {
		reader := &boundedFramingReader{remaining: []byte(boundedHeartbeatFrame[:length]), eofWithData: true}
		framer, err := newBoundedParser(reader, len(boundedHeartbeatFrame))
		if err != nil {
			t.Fatal(err)
		}
		storage := bytes.Repeat([]byte{0xa5}, len(boundedHeartbeatFrame))
		before := bytes.Clone(storage)
		frame, err := framer.ReadMessageInto(storage)
		expected := io.ErrUnexpectedEOF
		if length == 0 {
			expected = io.EOF
		}
		if err != expected || len(frame) != 0 || !bytes.Equal(storage, before) {
			t.Fatalf("prefix length=%d error=%v, want %v without output mutation", length, err, expected)
		}
	}
}

func TestBoundedParserConfiguration(t *testing.T) {
	for _, capacity := range []int{0, -1} {
		if _, err := newBoundedParser(strings.NewReader(""), capacity); err != errInvalidBoundedParser {
			t.Fatalf("capacity=%d error=%v", capacity, err)
		}
	}
	if _, err := newBoundedParser(nil, 128); err != errInvalidBoundedParser {
		t.Fatalf("nil reader error=%v", err)
	}
	if _, err := newParser(strings.NewReader(boundedHeartbeatFrame)).ReadMessageInto(make([]byte, 128)); err != errParserNotBounded {
		t.Fatalf("unbounded parser error=%v", err)
	}
}

type boundedNoProgressReader struct{ reads int }

func (reader *boundedNoProgressReader) Read([]byte) (int, error) {
	reader.reads++
	return 0, nil
}

func TestBoundedParserNoProgress(t *testing.T) {
	reader := &boundedNoProgressReader{}
	framer, err := newBoundedParser(reader, 128)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := framer.ReadMessageInto(make([]byte, 128)); err != io.ErrNoProgress || reader.reads != 100 {
		t.Fatalf("error=%v reads=%d, want ErrNoProgress after 100 empty reads", err, reader.reads)
	}
}

func TestBoundedFramingNoAllocations(t *testing.T) {
	_, messageBytes, _ := allocationBaselineFIX40Order(t)
	reader := &allocationBaselineRepeatingReader{message: messageBytes}
	framer, err := newBoundedParser(reader, len(messageBytes))
	if err != nil {
		t.Fatal(err)
	}
	storage := make([]byte, len(messageBytes))
	if allocations := testing.AllocsPerRun(1000, func() {
		frame, err := framer.ReadMessageInto(storage)
		if err != nil || !bytes.Equal(frame, messageBytes) {
			panic("bounded framing fixture failed")
		}
	}); allocations != 0 {
		t.Fatalf("allocations=%v, want zero", allocations)
	}
}

func BenchmarkBoundedFraming(b *testing.B) {
	_, messageBytes, _ := allocationBaselineFIX40Order(b)
	reader := &allocationBaselineRepeatingReader{message: messageBytes}
	framer, err := newBoundedParser(reader, len(messageBytes))
	if err != nil {
		b.Fatal(err)
	}
	storage := make([]byte, len(messageBytes))
	frame, err := framer.ReadMessageInto(storage)
	if err != nil || !bytes.Equal(frame, messageBytes) {
		b.Fatalf("warm-up frame=%q error=%v", frame, err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(messageBytes)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		frame, err = framer.ReadMessageInto(storage)
		if err != nil {
			b.Fatal(err)
		}
		allocationBaselineBytes = frame
	}
}

func assertBoundedFramingStructure(t *testing.T, frame []byte, capacity int) {
	t.Helper()
	if len(frame) > capacity || !bytes.HasPrefix(frame, []byte("8=")) {
		t.Fatalf("invalid returned frame size or prefix: %q", frame)
	}
	beginEnd := bytes.IndexByte(frame, '\x01')
	if beginEnd < 0 || len(frame)-beginEnd < 4 || string(frame[beginEnd+1:beginEnd+3]) != "9=" {
		t.Fatalf("missing second-field BodyLength: %q", frame)
	}
	lengthStart := beginEnd + 3
	lengthEnd := bytes.IndexByte(frame[lengthStart:], '\x01')
	if lengthEnd < 0 {
		t.Fatalf("unterminated BodyLength: %q", frame)
	}
	lengthEnd += lengthStart
	bodyLength, err := strconv.Atoi(string(frame[lengthStart:lengthEnd]))
	if err != nil || bodyLength <= 0 || bodyLength > capacity || len(frame)-(lengthEnd+1)-7 != bodyLength {
		t.Fatalf("returned frame does not match BodyLength: %q", frame)
	}
	checksum := frame[len(frame)-7:]
	if frame[len(frame)-8] != '\x01' || string(checksum[:3]) != "10=" || checksum[6] != '\x01' {
		t.Fatalf("checksum is not at the declared boundary: %q", frame)
	}
	for _, digit := range checksum[3:6] {
		if digit < '0' || digit > '9' {
			t.Fatalf("non-numeric framed checksum: %q", frame)
		}
	}
}

func FuzzBoundedFraming(f *testing.F) {
	const maximum = 4096
	for _, seed := range []struct {
		wire     string
		capacity int
		chunk    uint8
	}{
		{boundedHeartbeatFrame + boundedHeartbeatFrame, len(boundedHeartbeatFrame), 1},
		{allocationBaselineRepeatingGroupMessage + boundedHeartbeatFrame, maximum, 0},
		{boundedHeartbeatFrame, len(boundedHeartbeatFrame) - 1, 7},
		{boundedHeartbeatFrame[:len(boundedHeartbeatFrame)-1], maximum, 1},
		{"8=FIX.4.4\x019=999999999999999999999999999999\x01", maximum, 3},
		{"8=FIX.4.4\x019=-5\x0135=0\x0110=163\x01", maximum, 0},
		{"8=FIX.4.4\x019=4\x0135=0\x0110=163\x01" + boundedHeartbeatFrame, maximum, 0},
		{"8=FIX.4.4\x019=5\x0135=0\x0110=bad\x01", maximum, 0},
		{"", 1, 0},
	} {
		f.Add([]byte(seed.wire), uint16(seed.capacity-1), seed.chunk)
	}
	f.Fuzz(func(t *testing.T, input []byte, configured uint16, chunk uint8) {
		if len(input) > maximum {
			t.Skip()
		}
		capacity := int(configured)%maximum + 1
		reader := &boundedFramingReader{remaining: input, chunk: int(chunk), eofWithData: true}
		framer, err := newBoundedParser(reader, capacity)
		if err != nil {
			t.Fatal(err)
		}
		scratch := &framer.bigBuffer[0]
		storage := [2][]byte{make([]byte, capacity), make([]byte, capacity)}
		var previous, previousCopy []byte
		// A finite input cannot produce more nonempty frames than input bytes.
		// Alternating caller-owned destinations also checks the preceding frame
		// remains valid across a subsequent read, including one returning error.
		for iteration := 0; iteration <= len(input); iteration++ {
			destination := storage[iteration%2]
			for index := range destination {
				destination[index] = 0xa5
			}
			before := bytes.Clone(destination)
			frame, err := framer.ReadMessageInto(destination)
			if !bytes.Equal(previous, previousCopy) {
				t.Fatal("next read changed a previously returned caller-owned frame")
			}
			if &framer.bigBuffer[0] != scratch || len(framer.bigBuffer) != capacity || cap(framer.bigBuffer) != capacity || cap(frame) != capacity {
				t.Fatal("bounded framing changed fixed storage")
			}
			if err != nil {
				if len(frame) != 0 || !bytes.Equal(destination, before) {
					t.Fatal("failed read changed destination storage")
				}
				return
			}
			assertBoundedFramingStructure(t, frame, capacity)
			if &frame[0] != &destination[0] {
				t.Fatal("returned frame does not belong to the supplied destination")
			}
			previous, previousCopy = frame, bytes.Clone(frame)
		}
		t.Fatal("finite input produced an unbounded number of frames")
	})
}
