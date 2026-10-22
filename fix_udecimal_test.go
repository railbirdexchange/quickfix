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
	"bytes"
	"strings"
	"testing"

	decimal "github.com/quagmt/udecimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func BenchmarkFIXUDecimalRead(b *testing.B) {
	var dec FIXUDecimal
	byt := []byte("-124.3456")
	for i := 0; i < b.N; i++ {
		if err := dec.Read(byt); err != nil {
			b.FailNow()
		}
	}
}

func BenchmarkFIXUDecimalWrite(b *testing.B) {
	dec := FIXUDecimal{Decimal: decimal.MustParse("-124.3456"), Scale: 5}
	for i := 0; i < b.N; i++ {
		if byt := dec.Write(); len(byt) == 0 {
			b.FailNow()
		}
	}
}

func TestFIXUDecimalWrite(t *testing.T) {
	var tests = []struct {
		decimal  FIXUDecimal
		expected string
	}{
		{decimal: FIXUDecimal{Decimal: decimal.MustParse("-124.3456"), Scale: 4}, expected: "-124.3456"},
		{decimal: FIXUDecimal{Decimal: decimal.MustParse("-124.3456"), Scale: 5}, expected: "-124.34560"},
		{decimal: FIXUDecimal{Decimal: decimal.MustParse("-124.3456"), Scale: 0}, expected: "-124"},
	}

	for _, test := range tests {
		b := test.decimal.Write()
		assert.Equal(t, test.expected, string(b))
	}
}

func TestFIXUDecimalRead(t *testing.T) {
	var tests = []struct {
		bytes       string
		expected    decimal.Decimal
		expectError bool
	}{
		{bytes: "15", expected: decimal.MustParse("15")},
		{bytes: "15.000", expected: decimal.MustParse("15")},
		{bytes: "15.001", expected: decimal.MustParse("15.001")},
		{bytes: "-15.001", expected: decimal.MustParse("-15.001")},
		{bytes: "blah", expectError: true},
		{bytes: "+200.00", expected: decimal.MustParse("200")},
	}

	for _, test := range tests {
		var field FIXUDecimal

		err := field.Read([]byte(test.bytes))
		require.Equal(t, test.expectError, err != nil)

		if !test.expectError {
			assert.True(t, test.expected.Equal(field.Decimal), "Expected %s got %s", test.expected, field.Decimal)
		}
	}
}

func TestFIXUDecimalReadCompatibility(t *testing.T) {
	for _, input := range fixUDecimalReadFixtures() {
		t.Run(input, func(t *testing.T) {
			assertFIXUDecimalReadCompatibility(t, []byte(input))
		})
	}
	assertFIXUDecimalReadCompatibility(t, nil)
}

func FuzzFIXUDecimalReadCompatibility(f *testing.F) {
	for _, input := range fixUDecimalReadFixtures() {
		f.Add([]byte(input))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		assertFIXUDecimalReadCompatibility(t, input)
	})
}

func fixUDecimalReadFixtures() []string {
	return []string{
		"0", "-0", "+0.000", "15", "15.000", "15.001", "-15.001", "+200.00",
		"00015.00100", "-124.3456", "0.0000000000000000001",
		"-1234567890123456789.1234567890123456789",
		"340282366920938463463374607431768211455", // Largest uint128 coefficient.
		"340282366920938463463374607431768211456", // Arbitrary-precision overflow.
		"-340282366920938463463.374607431768211455",
		strings.Repeat("9", 200), strings.Repeat("9", 201),
		"", "+", "-", ".", "1.", ".1", "1.2.3", "1e3", " 1", "1 ", "blah",
		"0.12345678901234567890", "1\x00", "1\x01", "\xff", "１２",
	}
}

func assertFIXUDecimalReadCompatibility(t *testing.T, input []byte) {
	t.Helper()

	// Preserve the old parser's exact value, precision, error chain and error text.
	expected, expectedError := decimal.Parse(string(input))
	originalInput := bytes.Clone(input)
	field := FIXUDecimal{Decimal: decimal.MustParse("987.654"), Scale: 7}
	err := field.Read(input)
	require.Equal(t, expected, field.Decimal)
	require.Equal(t, expectedError, err)
	require.Equal(t, uint8(7), field.Scale)
	require.Equal(t, originalInput, input, "Read must not modify the borrowed input")
	if expectedError != nil {
		require.Equal(t, expectedError.Error(), err.Error())
		require.True(t, field.IsZero(), "a failed read must clear the previous value")
	}
	for _, sentinel := range []error{decimal.ErrEmptyString, decimal.ErrMaxStrLen} {
		if expectedError == sentinel {
			require.Same(t, sentinel, err, "preserve direct sentinel comparisons")
		}
	}

	// The caller may reuse its input slot immediately after Read returns.
	for index := range input {
		input[index] = 'x'
	}
	require.Equal(t, expected, field.Decimal)
	if expectedError != nil {
		require.Equal(t, expectedError.Error(), err.Error())
	}

	// The same field must also recover after an invalid or overflow input.
	require.NoError(t, field.Read([]byte("1.25")))
	require.Equal(t, decimal.MustParse("1.25"), field.Decimal)
	require.Equal(t, uint8(7), field.Scale)
}
