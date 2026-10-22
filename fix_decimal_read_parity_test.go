package quickfix

import (
	"fmt"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestFIXDecimalReadRepresentation(t *testing.T) {
	for _, fixture := range []struct {
		input       string
		coefficient string
		exponent    int32
		scale       int32
		wire        string
	}{
		{input: "100", coefficient: "100", exponent: 0, scale: 0, wire: "100"},
		{input: "123.45", coefficient: "12345", exponent: -2, scale: 2, wire: "123.45"},
		{input: "-124.3456", coefficient: "-1243456", exponent: -4, scale: 2, wire: "-124.35"},
		{input: "-124.3456", coefficient: "-1243456", exponent: -4, scale: 0, wire: "-124"},
		{input: "+0015.000", coefficient: "15000", exponent: -3, scale: 4, wire: "15.0000"},
		{input: "-0.0000", coefficient: "0", exponent: -4, scale: 2, wire: "0.00"},
		{input: "+0", coefficient: "0", exponent: 0, scale: 0, wire: "0"},
		{input: ".5", coefficient: "5", exponent: -1, scale: 2, wire: "0.50"},
		{input: "-.5", coefficient: "-5", exponent: -1, scale: 2, wire: "-0.50"},
		{input: "5.", coefficient: "5", exponent: 0, scale: 2, wire: "5.00"},
		{input: "1.2345e+2", coefficient: "12345", exponent: -2, scale: 2, wire: "123.45"},
		{input: "1.2300E-2", coefficient: "12300", exponent: -6, scale: 6, wire: "0.012300"},
		{input: "9223372036854775807", coefficient: "9223372036854775807", exponent: 0, scale: 0, wire: "9223372036854775807"},
		{input: "-9223372036854775808", coefficient: "-9223372036854775808", exponent: 0, scale: 0, wire: "-9223372036854775808"},
		{input: "9223372036854775808.12345", coefficient: "922337203685477580812345", exponent: -5, scale: 5, wire: "9223372036854775808.12345"},
	} {
		t.Run(fmt.Sprintf("%s/scale_%d", fixture.input, fixture.scale), func(t *testing.T) {
			field := FIXDecimal{Decimal: decimal.New(777, -2), Scale: fixture.scale}
			require.NoError(t, field.Read([]byte(fixture.input)))
			require.Equal(t, fixture.coefficient, field.Coefficient().String())
			require.Equal(t, fixture.exponent, field.Exponent())
			require.Equal(t, fixture.scale, field.Scale)
			require.Equal(t, fixture.wire, string(field.Write()))
		})
	}
}

func requireDecimalReadParity(t *testing.T, input []byte) {
	t.Helper()
	expected, expectedError := decimal.NewFromString(string(input))
	field := FIXDecimal{Decimal: decimal.New(777, -2), Scale: 7}
	err := field.Read(input)
	require.Equal(t, int32(7), field.Scale, "input %q", input)
	if expectedError != nil {
		require.EqualError(t, err, expectedError.Error(), "input %q", input)
		require.Equal(t, decimal.Decimal{}, field.Decimal, "failed reads replace the previous decimal: %q", input)
		return
	}
	require.NoError(t, err, "input %q", input)
	require.Equal(t, expected.Exponent(), field.Exponent(), "input %q", input)
	require.Zero(t, expected.Coefficient().Cmp(field.Coefficient()), "input %q", input)
}

func TestFIXDecimalReadLegacyParity(t *testing.T) {
	for _, input := range []string{
		"", "+", "-", ".", "+.", "-.", "1..0", "1.2.3", "--1", "++1", "1-2", "1+2",
		" 1", "1 ", "1\t", "1\n", "\x001", "1\x00", "１", "NaN", "Inf", "-Inf", "0x10", "1_000", "1,000",
		"e1", "1e", "1e+", "1e-", "1e1.0", "1e2e3", "1E2E3", "1.2.3e4", "1.2.3e", "1e2147483648", "1e-2147483649",
		"1e2147483647", "1e-2147483648", "1.0e-2147483648", "0e-2147483648", "0.0e-2147483648", "1.0e2147483647",
		"1e+0002", "-0e-3", "+.123e2", "123.e-2", "+000000000000000000000001.2300000000000000000",
		"9223372036854775808", "-9223372036854775809", "18446744073709551615", "18446744073709551616",
		"1234567890123456789012345678901234567890.12345678901234567890",
		strings.Repeat("0", 128), "-" + strings.Repeat("0", 128), "0." + strings.Repeat("0", 128) + "1",
	} {
		requireDecimalReadParity(t, []byte(input))
	}
	// Cover coefficient boundaries with signs, decimal placement and exponents;
	// compare stored scale as well as numeric value, including trailing zeroes.
	for _, digits := range []string{"0", "1", "9", "10", "99", "00100", "123456789012345678", "9223372036854775807", "9223372036854775808", "9223372036854775809"} {
		for _, sign := range []string{"", "+", "-"} {
			for point := 0; point <= len(digits); point++ {
				coefficient := sign + digits[:point] + "." + digits[point:]
				for _, exponent := range []string{"", "e0", "E+2", "e-4", "e2147483647", "e-2147483648"} {
					requireDecimalReadParity(t, []byte(coefficient+exponent))
				}
			}
		}
	}
}

func TestFIXDecimalReadRetainsOwnedValues(t *testing.T) {
	input := []byte("123.4500")
	var field FIXDecimal
	require.NoError(t, field.Read(input))
	first := field.Decimal
	message := NewMessage()
	message.Body.SetBytes(38, input)
	fromGetter, reject := readDecimalQuantity(message)
	require.Nil(t, reject)
	copy(input, "987.6500")
	require.NoError(t, field.Read(input))
	second := field.Decimal
	require.Error(t, field.Read([]byte("invalid")))
	require.Equal(t, "1234500", first.Coefficient().String())
	require.Equal(t, int32(-4), first.Exponent())
	require.Equal(t, "1234500", fromGetter.Coefficient().String())
	require.Equal(t, int32(-4), fromGetter.Exponent())
	require.Equal(t, "9876500", second.Coefficient().String())
	require.Equal(t, int32(-4), second.Exponent())
}

func FuzzFIXDecimalReadParity(fuzz *testing.F) {
	for _, input := range []string{"123.4500", "-0.0000", "+.123e2", "9223372036854775807", "-9223372036854775808", "9223372036854775808.12345", "1e2147483648", "1..2"} {
		fuzz.Add([]byte(input))
	}
	fuzz.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 256 {
			t.Skip()
		}
		requireDecimalReadParity(t, input)
	})
}
