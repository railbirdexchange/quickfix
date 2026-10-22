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

	"github.com/quagmt/udecimal"
)

// FIXUDecimal is a FIX Float Value that implements an arbitrary precision fixed-point udecimal.  Implements FieldValue.
type FIXUDecimal struct {
	udecimal.Decimal

	// Scale is the number of digits after the decimal point when Writing the field value as a FIX value.
	Scale uint8
}

func (d FIXUDecimal) Write() []byte {
	return []byte(d.Decimal.Trunc(d.Scale).StringFixed(d.Scale))
}

func (d *FIXUDecimal) Read(bytes []byte) error {
	if err := d.Decimal.UnmarshalText(bytes); err != nil {
		// UnmarshalText wraps the Parse error once; preserve Read's existing
		// error text and direct sentinel comparisons.
		return errors.Unwrap(err)
	}
	return nil
}
