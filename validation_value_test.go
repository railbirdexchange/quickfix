package quickfix

import (
	"bytes"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

func TestValidFieldValueFloatRoundingBoundary(t *testing.T) {
	// Halfway between MaxFloat64 and 2^1024 rounds to infinity. Derive
	// the decimal fixture independently rather than copying a production limit.
	boundary := new(big.Int).Sub(
		new(big.Int).Lsh(big.NewInt(1), 1024),
		new(big.Int).Lsh(big.NewInt(1), 970),
	)
	if boundary.String() != floatOverflowMidpoint {
		t.Fatal("decimal overflow boundary does not equal 2^1024 - 2^970")
	}
	for _, delta := range []int64{-1, 0, 1} {
		value := new(big.Int).Add(boundary, big.NewInt(delta)).String()
		for _, prefix := range []string{"", "-", "000", "-000"} {
			for _, suffix := range []string{"", ".", ".0", ".999999999999999999999999999999999999"} {
				input := []byte(prefix + value + suffix)
				var reader FIXFloat
				legacy := reader.Read(input) == nil
				if got := validFieldValue("FLOAT", input); got != legacy || got != (delta < 0) {
					t.Fatalf("boundary %+d, %q: valid=%t legacy=%t want=%t", delta, input, got, legacy, delta < 0)
				}
			}
		}
	}
}

func TestValidFieldValueFloatLongFractionalBoundary(t *testing.T) {
	boundary := new(big.Int).Sub(
		new(big.Int).Lsh(big.NewInt(1), 1024),
		new(big.Int).Lsh(big.NewInt(1), 970),
	)
	// strconv's decimal fallback stores 800 significant digits and a sticky
	// flag for discarded nonzero digits. With this 309-digit integer part,
	// 491/492 fraction digits straddle that capacity; 801/1000 also exercise
	// fractions longer than its entire decimal storage.
	for _, fractionalDigits := range []int{491, 492, 801, 1000} {
		for _, delta := range []int64{-1, 0} {
			integer := new(big.Int).Add(boundary, big.NewInt(delta)).String()
			for _, prefix := range []string{"", "-"} {
				for _, fraction := range []string{
					strings.Repeat("9", fractionalDigits),
					strings.Repeat("0", fractionalDigits),
					strings.Repeat("0", fractionalDigits) + "1",
				} {
					input := []byte(prefix + integer + "." + fraction)
					var reader FIXFloat
					legacy := reader.Read(input) == nil
					if got := validFieldValue("FLOAT", input); got != legacy || got != (delta < 0) {
						t.Fatalf("boundary %+d, sign=%q, fraction length=%d, last=%q: valid=%t legacy=%t want=%t", delta, prefix, len(fraction), fraction[len(fraction)-1:], got, legacy, delta < 0)
					}
				}
			}
		}
	}
}

func TestValidFieldValueTypeMapping(t *testing.T) {
	stringKinds := []string{"MULTIPLESTRINGVALUE", "MULTIPLEVALUESTRING", "MULTIPLECHARVALUE", "CHAR", "CURRENCY", "DATA", "MONTHYEAR", "LOCALMKTDATE", "DATE", "EXCHANGE", "LANGUAGE", "XMLDATA", "COUNTRY", "UTCTIMEONLY", "UTCDATEONLY", "UTCDATE", "TZTIMEONLY", "TZTIMESTAMP", "STRING"}
	for _, kind := range stringKinds {
		for _, value := range [][]byte{nil, []byte("not a date or time"), {0, 0xff, '\x01'}} {
			if !validFieldValue(kind, value) {
				t.Errorf("string-mapped kind %s rejected %q", kind, value)
			}
		}
	}
	for _, kind := range []string{"LENGTH", "DAYOFMONTH", "NUMINGROUP", "SEQNUM", "INT"} {
		if !validFieldValue(kind, []byte("-999999999999999999999999999999999999999999")) || validFieldValue(kind, []byte("+1")) {
			t.Errorf("integer mapping changed for %s", kind)
		}
	}
	for _, kind := range []string{"QTY", "QUANTITY", "AMT", "PRICE", "PRICEOFFSET", "PERCENTAGE", "FLOAT"} {
		if !validFieldValue(kind, []byte("-.5")) || validFieldValue(kind, []byte("1e2")) {
			t.Errorf("float mapping changed for %s", kind)
		}
	}
	for _, kind := range []string{"UTCTIMESTAMP", "TIME"} {
		if !validFieldValue(kind, []byte("20000229-23:59:59,123")) || validFieldValue(kind, []byte("19000229-23:59:59.123")) {
			t.Errorf("timestamp mapping changed for %s", kind)
		}
	}
	if validFieldValue("UNSUPPORTED_DICTIONARY_TYPE", []byte("anything")) {
		t.Fatal("unknown dictionary type accepted")
	}
}

func TestValidFieldValueReaderDifferential(t *testing.T) {
	cases := map[string][]string{
		"BOOLEAN":      {"", "Y", "N", "y", "n", "true", "0", "YN", " Y", "Y\x00"},
		"INT":          {"", "0", "-0", "-1", "123", "001", "-001", "-", "+1", "1.0", "--1", "1-", "1 2", " 1", "1\n", "9999999999999999999999999999999999999999999999999999999999999999999"},
		"FLOAT":        {"", "0", "-0", "0.", ".0", "-.0", "-123.456", "001.02", "-", ".", "-.", "1e3", "1E3", "+1", "NaN", "Inf", "-Inf", "0x1p2", "1_000", "1.2.3", "1-2", "1\n", "1 2", strings.Repeat("9", 308), strings.Repeat("9", 309), "0." + strings.Repeat("0", 1000) + "1", strings.Repeat("0", 1000) + "1.5"},
		"UTCTIMESTAMP": {"", "00000101-00:00:00", "00000229-00:00:00", "99991231-23:59:59", "20000229-12:34:56", "19000229-12:34:56", "20010229-12:34:56", "20260431-12:34:56", "20260001-00:00:00", "20261301-00:00:00", "20260100-00:00:00", "20260101-24:00:00", "20260101-23:60:00", "20260101-23:59:60", "20260101-1:02:03", "20260101-01:02:03.12", "20260101-01:02:03.123", "20260101-01:02:03,123", "20260101-01:02:03.123456", "20260101-01:02:03,123456", "20260101-01:02:03.123456789", "20260101-01:02:03,123456789"},
	}
	for kind, values := range cases {
		for _, value := range values {
			assertFieldReaderMatches(t, kind, []byte(value))
		}
	}
	for _, fixture := range []string{"20260228-09:08:07", "20240229-23:59:59.123", "00000101-00:00:00.123456", "99991231-23:59:59.123456789"} {
		value := []byte(fixture)
		for index, original := range value {
			for _, replacement := range []byte{'0', '1', '2', '9', 'a', '-', '+', ':', '.', ',', ' ', 0, 0xff} {
				value[index] = replacement
				assertFieldReaderMatches(t, "UTCTIMESTAMP", value)
			}
			value[index] = original
		}
	}
}

func assertFieldReaderMatches(t *testing.T, kind string, value []byte) {
	t.Helper()
	var err error
	switch kind {
	case "BOOLEAN":
		var reader FIXBoolean
		err = reader.Read(value)
	case "INT":
		// Legacy FIXInt.Read panics on empty input; validateField rejects it
		// before calling the reader. The direct helper safely rejects it too.
		if len(value) == 0 {
			if validFieldValue(kind, value) {
				t.Fatal("empty integer accepted")
			}
			return
		}
		var reader FIXInt
		err = reader.Read(value)
	case "FLOAT":
		var reader FIXFloat
		err = reader.Read(value)
	case "UTCTIMESTAMP":
		var reader FIXUTCTimestamp
		err = reader.Read(value)
	default:
		t.Fatalf("unsupported differential reader %s", kind)
	}
	if got := validFieldValue(kind, value); got != (err == nil) {
		t.Fatalf("%s %q: valid=%t, reader error=%v", kind, value, got, err)
	}
}

func TestValidEnumValuesPreservesSplitSemantics(t *testing.T) {
	for _, kind := range []string{"STRING", "MULTIPLESTRINGVALUE", "MULTIPLEVALUESTRING", "MULTIPLECHARVALUE"} {
		for _, allowEmpty := range []bool{false, true} {
			fieldType := &datadictionary.FieldType{Type: kind, Enums: map[string]datadictionary.Enum{"A": {}, "B": {}, "A\tB": {}, "A B": {}}}
			if allowEmpty {
				fieldType.Enums[""] = datadictionary.Enum{}
			}
			for _, value := range []string{"", "A", "A B", " A", "A ", "A  B", " ", "  ", "A\tB", "A\nB", "A\u00a0B", "C", "A C"} {
				components := [][]byte{[]byte(value)}
				if kind != "STRING" {
					components = bytes.Split([]byte(value), []byte{' '})
				}
				expected := true
				for _, component := range components {
					if _, found := fieldType.Enums[string(component)]; !found {
						expected = false
					}
				}
				if got := validEnumValues(fieldType, []byte(value)); got != expected {
					t.Errorf("%s allowEmpty=%t value=%q: valid=%t, want=%t", kind, allowEmpty, value, got, expected)
				}
			}
		}
	}
	if !validEnumValues(&datadictionary.FieldType{}, []byte("anything")) || validEnumValues(nil, nil) {
		t.Fatal("empty or absent enum definition handled incorrectly")
	}
}

func TestValidationValueHelpersDoNotAllocate(t *testing.T) {
	cases := []struct{ kind, value string }{
		{"STRING", strings.Repeat("x", 4096)}, {"BOOLEAN", "Y"},
		{"INT", "-" + strings.Repeat("9", 1024)},
		{"FLOAT", "123.456"}, {"FLOAT", strings.Repeat("9", 308)},
		{"FLOAT", "1" + strings.Repeat("0", 308)},
		{"FLOAT", "0." + strings.Repeat("0", 1024) + "1"},
		{"UTCTIMESTAMP", "20261015-12:34:56.123456789"},
	}
	for index, fixture := range cases {
		t.Run(fmt.Sprintf("%s/%d", fixture.kind, index), func(t *testing.T) {
			value := []byte(fixture.value)
			if allocations := testing.AllocsPerRun(1000, func() {
				if !validFieldValue(fixture.kind, value) {
					panic("valid allocation fixture rejected")
				}
			}); allocations != 0 {
				t.Fatalf("allocations = %v, want 0", allocations)
			}
		})
	}
	longToken := strings.Repeat("x", 128)
	fieldType := &datadictionary.FieldType{Type: "MULTIPLESTRINGVALUE", Enums: map[string]datadictionary.Enum{"A": {}, longToken: {}}}
	value := []byte("A " + longToken + " A")
	if allocations := testing.AllocsPerRun(1000, func() {
		if !validEnumValues(fieldType, value) {
			panic("valid enum fixture rejected")
		}
	}); allocations != 0 {
		t.Fatalf("enum allocations = %v, want 0", allocations)
	}
}

func FuzzValidFieldValueMatchesReader(f *testing.F) {
	for _, fixture := range []struct {
		selector uint8
		value    string
	}{
		{0, "Y"}, {1, "-123"}, {2, "-.5"},
		{2, floatOverflowMidpoint}, {2, strings.Repeat("9", 308)},
		{3, "20240229-12:34:56.123456789"},
	} {
		f.Add(fixture.selector, []byte(fixture.value))
	}
	f.Fuzz(func(t *testing.T, selector uint8, value []byte) {
		kinds := [...]string{"BOOLEAN", "INT", "FLOAT", "UTCTIMESTAMP"}
		assertFieldReaderMatches(t, kinds[int(selector)%len(kinds)], value)
	})
}
