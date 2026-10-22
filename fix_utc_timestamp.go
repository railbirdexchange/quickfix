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
	if parsed, precision, ok := readCanonicalUTCTimestamp(bytes); ok {
		f.Time, f.Precision = parsed, precision
		return nil
	}
	// Keep the general parser for noncanonical inputs and errors. In particular,
	// time.Parse accepts comma fractions, and its error and receiver behavior are
	// part of the existing reader's contract.
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

// readCanonicalUTCTimestamp handles only the four exact FIX wire layouts. Every
// component is range checked before time.Date, which otherwise normalizes invalid
// dates. A false result delegates acceptance and error reporting to the old reader.
func readCanonicalUTCTimestamp(value []byte) (time.Time, TimestampPrecision, bool) {
	var precision TimestampPrecision
	var fractionScale int
	switch len(value) {
	case 17:
		precision = Seconds
	case 21:
		precision, fractionScale = Millis, 1000000
	case 24:
		precision, fractionScale = Micros, 1000
	case 27:
		precision, fractionScale = Nanos, 1
	default:
		return time.Time{}, 0, false
	}
	if value[8] != '-' || value[11] != ':' || value[14] != ':' || len(value) > 17 && value[17] != '.' {
		return time.Time{}, 0, false
	}
	date := 0
	for _, digit := range value[:8] {
		digit -= '0'
		if digit > 9 {
			return time.Time{}, 0, false
		}
		date = date*10 + int(digit)
	}
	year, month, day := date/10000, date/100%100, date%100
	if month < 1 || month > 12 || day < 1 {
		return time.Time{}, 0, false
	}
	maxDay := 31
	switch month {
	case 4, 6, 9, 11:
		maxDay = 30
	case 2:
		maxDay = 28
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			maxDay = 29
		}
	}
	if day > maxDay {
		return time.Time{}, 0, false
	}
	hour, hourOK := timestampTwoDigits(value, 9)
	minute, minuteOK := timestampTwoDigits(value, 12)
	second, secondOK := timestampTwoDigits(value, 15)
	if !hourOK || !minuteOK || !secondOK || hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, 0, false
	}
	nanosecond := 0
	if len(value) > 17 {
		for _, digit := range value[18:] {
			digit -= '0'
			if digit > 9 {
				return time.Time{}, 0, false
			}
			nanosecond = nanosecond*10 + int(digit)
		}
		nanosecond *= fractionScale
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, nanosecond, time.UTC), precision, true
}

func timestampTwoDigits(value []byte, offset int) (int, bool) {
	first, second := value[offset]-'0', value[offset+1]-'0'
	return int(first)*10 + int(second), first <= 9 && second <= 9
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
