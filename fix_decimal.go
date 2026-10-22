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
	"math"

	"github.com/shopspring/decimal"
)

// FIXDecimal is a FIX Float Value that implements an arbitrary precision fixed-point decimal.  Implements FieldValue.
type FIXDecimal struct {
	decimal.Decimal

	// Scale is the number of digits after the decimal point when Writing the field value as a FIX value.
	Scale int32
}

func (d FIXDecimal) Write() []byte {
	return []byte(d.Decimal.StringFixed(d.Scale))
}

func (d *FIXDecimal) Read(bytes []byte) (err error) {
	if coefficient, exponent, ok := decimalInt64Bytes(bytes); ok {
		// New gives the caller an owned coefficient. Reusing its backing across
		// reads would mutate decimal values retained from a previous getter.
		d.Decimal = decimal.New(coefficient, exponent)
		return nil
	}
	d.Decimal, err = decimal.NewFromString(string(bytes))
	return
}

// decimalInt64Bytes handles ordinary decimal text without temporary strings.
// Scientific notation, larger coefficients and invalid input use the existing
// parser so its arbitrary precision and exact error behavior remain unchanged.
func decimalInt64Bytes(value []byte) (int64, int32, bool) {
	if len(value) == 0 || len(value) > math.MaxInt32 {
		return 0, 0, false
	}
	start := 0
	negative := value[0] == '-'
	limit := uint64(math.MaxInt64)
	if negative || value[0] == '+' {
		start = 1
		if negative {
			limit++
		}
	}
	var coefficient uint64
	point := -1
	hasDigit := false
	for index := start; index < len(value); index++ {
		character := value[index]
		if character == '.' && point < 0 {
			point = index
			continue
		}
		if character < '0' || character > '9' {
			return 0, 0, false
		}
		digit := uint64(character - '0')
		if coefficient > limit/10 || coefficient == limit/10 && digit > limit%10 {
			return 0, 0, false
		}
		coefficient = coefficient*10 + digit
		hasDigit = true
	}
	if !hasDigit {
		return 0, 0, false
	}
	var exponent int32
	if point >= 0 {
		exponent = int32(point + 1 - len(value))
	}
	if negative {
		return -int64(coefficient), exponent, true
	}
	return int64(coefficient), exponent, true
}
