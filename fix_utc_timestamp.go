// Copyright (c) quickfixengine.org  All rights reserved.
//
// This file may be distributed under the terms of the quickfixengine.org
// license as defined by quickfixengine.org and appearing in the file
// LICENSE included in the packaging of this file.
//
// This file is provided AS IS with NO WARRANTY OF ANY KIND, INCLUDING
// THE WARRANTY OF DESIGN, MERCHANTABILITY AND FITNESS FOR A
// PARTICULAR PURPOSE.
//
// See http://www.quickfixengine.org/LICENSE for licensing information.
//
// Contact ask@quickfixengine.org if any conditions of this licensing
// are not clear to you.

package quickfix

import (
	"errors"
	"time"
)

// TimestampPrecision defines the precision used by FIXUTCTimestamp.
type TimestampPrecision int

// All TimestampPrecisions supported by FIX.
const (
	Millis TimestampPrecision = iota
	Seconds
	Micros
	Nanos
)

// FIXUTCTimestamp is a FIX UTC Timestamp value, implements FieldValue.
type FIXUTCTimestamp struct {
	time.Time
	Precision TimestampPrecision
}

const (
	utcTimestampMillisFormat  = "20060102-15:04:05.000"
	utcTimestampSecondsFormat = "20060102-15:04:05"
	utcTimestampMicrosFormat  = "20060102-15:04:05.000000"
	utcTimestampNanosFormat   = "20060102-15:04:05.000000000"
)

func (f *FIXUTCTimestamp) Read(bytes []byte) (err error) {
	switch len(bytes) {
	// Seconds.
	case 17:
		f.Time, err = time.Parse(utcTimestampSecondsFormat, string(bytes))
		f.Precision = Seconds

	// Millis.
	case 21:
		f.Time, err = time.Parse(utcTimestampMillisFormat, string(bytes))
		f.Precision = Millis

	// micros
	case 24:
		f.Time, err = time.Parse(utcTimestampMicrosFormat, string(bytes))
		f.Precision = Micros

	// nanos
	case 27:
		f.Time, err = time.Parse(utcTimestampNanosFormat, string(bytes))
		f.Precision = Nanos

	default:
		err = errors.New("Invalid Value for Timestamp: " + string(bytes))
	}

	return
}

func (f FIXUTCTimestamp) Write() []byte {
	var scratch [64]byte
	encoded, _ := f.AppendTo(scratch[:0])
	return append([]byte(nil), encoded...)
}

// AppendTo appends the UTC timestamp with its configured precision to
// caller-owned storage. Insufficient capacity returns ErrBufferTooSmall and
// leaves dst unchanged; the output buffer is never grown.
func (f FIXUTCTimestamp) AppendTo(dst []byte) ([]byte, error) {
	// A signed int-sized year plus the longest FIX timestamp suffix fits in
	// 64 bytes, including years outside the usual four-digit FIX date range.
	var scratch [64]byte
	encoded := f.UTC().AppendFormat(scratch[:0], f.layout())
	if len(encoded) > cap(dst)-len(dst) {
		return dst, ErrBufferTooSmall
	}
	return append(dst, encoded...), nil
}

func (f FIXUTCTimestamp) layout() string {
	switch f.Precision {
	case Seconds:
		return utcTimestampSecondsFormat
	case Micros:
		return utcTimestampMicrosFormat
	case Nanos:
		return utcTimestampNanosFormat
	}
	return utcTimestampMillisFormat
}
