package quickfix

import "github.com/quickfixgo/quickfix/datadictionary"

// validFieldValue preserves the existing validator's type-to-reader mapping.
// Unknown dictionary types are invalid rather than constructing a nil reader.
// The caller separately rejects empty values before performing type validation.
func validFieldValue(kind string, value []byte) bool {
	switch kind {
	case "MULTIPLESTRINGVALUE", "MULTIPLEVALUESTRING", "MULTIPLECHARVALUE",
		"CHAR", "CURRENCY", "DATA", "MONTHYEAR", "LOCALMKTDATE", "DATE",
		"EXCHANGE", "LANGUAGE", "XMLDATA", "COUNTRY", "UTCTIMEONLY",
		"UTCDATEONLY", "UTCDATE", "TZTIMEONLY", "TZTIMESTAMP", "STRING":
		return true
	case "BOOLEAN":
		return len(value) == 1 && (value[0] == 'Y' || value[0] == 'N')
	case "LENGTH", "DAYOFMONTH", "NUMINGROUP", "SEQNUM", "INT":
		if len(value) > 0 && value[0] == '-' {
			value = value[1:]
		}
		return validationDigits(value)
	case "UTCTIMESTAMP", "TIME":
		return validTimestampValue(value)
	case "QTY", "QUANTITY", "AMT", "PRICE", "PRICEOFFSET", "PERCENTAGE", "FLOAT":
		return validFloatValue(value)
	}
	return false
}

// validTimestampValue preserves the public reader's length-selected time.Parse
// acceptance without constructing errors that boolean validation discards.
func validTimestampValue(value []byte) bool {
	switch len(value) {
	case 17, 21, 24, 27:
	default:
		return false
	}
	if len(value) == 17 || value[17] == '.' && value[18] != '+' && value[18] != '-' {
		_, _, ok := readCanonicalUTCTimestamp(value)
		return ok
	}
	if value[17] != '.' && value[17] != ',' {
		return false
	}

	// time.Parse accepts comma fractions and parses the fixed-width fraction
	// with signed atoi. A leading plus or negative zero can be normalized
	// without changing its width; negative nonzero fractions are rejected.
	var normalized [27]byte
	copy(normalized[:], value)
	normalized[17] = '.'
	switch value[18] {
	case '-':
		for _, digit := range value[19:] {
			if digit != '0' {
				return false
			}
		}
		normalized[18] = '0'
	case '+':
		normalized[18] = '0'
	}
	_, _, ok := readCanonicalUTCTimestamp(normalized[:len(value)])
	return ok
}

// validEnumValues deliberately preserves empty components. bytes.Fields would
// change the existing bytes.Split(value, []byte{' '}) behavior for whitespace.
func validEnumValues(fieldType *datadictionary.FieldType, value []byte) bool {
	if fieldType == nil {
		return false
	}
	if len(fieldType.Enums) == 0 {
		return true
	}
	switch fieldType.Type {
	case "MULTIPLESTRINGVALUE", "MULTIPLEVALUESTRING", "MULTIPLECHARVALUE":
		start := 0
		for index := 0; index <= len(value); index++ {
			if index != len(value) && value[index] != ' ' {
				continue
			}
			if _, found := fieldType.Enums[string(value[start:index])]; !found {
				return false
			}
			start = index + 1
		}
		return true
	default:
		_, found := fieldType.Enums[string(value)]
		return found
	}
}

func validationDigits(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// floatOverflowMidpoint is 2^1024 - 2^970: the exact midpoint between the
// largest finite float64 and 2^1024. ParseFloat rounds this midpoint to infinity
// and returns ErrRange (strconv/atof.go floatBits and decimal.go shouldRoundUp).
// Decimal magnitudes below it remain finite, including values slightly larger
// than MaxFloat64. No fractional part can cross this integer boundary.
//
// A grammar scan followed by ParseFloat(string(value), 64) allocates for valid
// long inputs. Comparing digits avoids both that copy and numeric conversion;
// validation needs only the range result, not the rounded float value.
const floatOverflowMidpoint = "179769313486231580793728971405303415079934132710037826936173778980444968292764750946649017977587207096330286416692887910946555547851940402630657488671505820681908902000708383676273854845817711531764475730270069855571366959622842914819860834936475292719074168444365510704342711559699508093042880177904174497792"

func validFloatValue(value []byte) bool {
	if len(value) > 0 && value[0] == '-' {
		value = value[1:]
	}
	integerEnd := len(value)
	firstNonzero := -1
	hasDigit := false
	hasPoint := false
	for index, digit := range value {
		if digit == '.' {
			if hasPoint {
				return false
			}
			hasPoint = true
			integerEnd = index
			continue
		}
		if digit < '0' || digit > '9' {
			return false
		}
		hasDigit = true
		if !hasPoint && firstNonzero < 0 && digit != '0' {
			firstNonzero = index
		}
	}
	if !hasDigit {
		return false
	}
	if firstNonzero < 0 {
		return true
	}
	significantDigits := integerEnd - firstNonzero
	if significantDigits != len(floatOverflowMidpoint) {
		return significantDigits < len(floatOverflowMidpoint)
	}
	for index := 0; index < significantDigits; index++ {
		if digit := value[firstNonzero+index]; digit != floatOverflowMidpoint[index] {
			return digit < floatOverflowMidpoint[index]
		}
	}
	return false
}
