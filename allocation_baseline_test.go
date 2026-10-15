// Copyright (c) quickfixengine.org  All rights reserved.
//
// This file may be distributed under the terms of the quickfixengine.org
// license as defined by the quickfixengine.org and appearing in the file
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
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

var (
	allocationBaselineBytes []byte
	allocationBaselineFrame *bytes.Buffer
)

const allocationBaselineRepeatingGroupMessage = "8=FIX.4.4\0019=210\00135=D\00134=2\001347=UTF-8\00152=20231231-20:19:41\00149=01001\00150=01001a\00156=TEST\00144=12\00111=13976\0011=10100400\00121=1\001386=1\001336=NOPL\00155=SYMABC\00154=1\00160=20231231-20:19:41\00138=1\00140=2\00159=0\001453=1\001448=4501\001447=D\001452=28\001354=6\001355=Public\00110=104\001"

func TestAllocationBaselineFixtures(t *testing.T) {
	message, messageBytes, dictionary := allocationBaselineFIX40Order(t)

	parsed := NewMessage()
	if err := ParseMessageWithDataDictionary(parsed, bytes.NewBuffer(messageBytes), dictionary, dictionary); err != nil {
		t.Fatalf("parse allocation baseline message: %v", err)
	}
	if reject := NewValidator(defaultValidatorSettings, dictionary, nil).Validate(parsed); reject != nil {
		t.Fatalf("validate allocation baseline message: %v", reject)
	}
	if got := message.build(); !bytes.Equal(got, messageBytes) {
		t.Fatalf("build allocation baseline message: got %q, want %q", got, messageBytes)
	}

	groupDictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatalf("parse allocation baseline dictionary: %v", err)
	}
	if err := ParseMessageWithDataDictionary(
		NewMessage(),
		bytes.NewBufferString(allocationBaselineRepeatingGroupMessage),
		groupDictionary,
		groupDictionary,
	); err != nil {
		t.Fatalf("parse allocation baseline repeating group message: %v", err)
	}
}

func BenchmarkAllocationBaseline(b *testing.B) {
	b.Run("parse/reused_message_without_dictionary", benchmarkAllocationBaselineParseWithoutDictionary)
	b.Run("parse/reused_message_with_dictionary", benchmarkAllocationBaselineParse)
	b.Run("parse/reused_message_with_repeating_group", benchmarkAllocationBaselineParseRepeatingGroup)
	b.Run("validate/valid_new_order_single", benchmarkAllocationBaselineValidate)
	b.Run("build/new_order_single", benchmarkAllocationBaselineBuild)
	b.Run("frame/read_steady_state", benchmarkAllocationBaselineReadFrame)
}

func benchmarkAllocationBaselineParseWithoutDictionary(b *testing.B) {
	_, messageBytes, _ := allocationBaselineFIX40Order(b)
	message := NewMessage()
	rawMessage := bytes.NewBuffer(messageBytes)
	if err := ParseMessage(message, rawMessage); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(messageBytes)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ParseMessage(message, rawMessage); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkAllocationBaselineParse(b *testing.B) {
	_, messageBytes, dictionary := allocationBaselineFIX40Order(b)
	message := NewMessage()
	rawMessage := bytes.NewBuffer(messageBytes)
	if err := ParseMessageWithDataDictionary(message, rawMessage, dictionary, dictionary); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(messageBytes)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ParseMessageWithDataDictionary(message, rawMessage, dictionary, dictionary); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkAllocationBaselineParseRepeatingGroup(b *testing.B) {
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		b.Fatal(err)
	}

	messageBytes := []byte(allocationBaselineRepeatingGroupMessage)
	message := NewMessage()
	rawMessage := bytes.NewBuffer(messageBytes)
	if err := ParseMessageWithDataDictionary(message, rawMessage, dictionary, dictionary); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(messageBytes)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ParseMessageWithDataDictionary(message, rawMessage, dictionary, dictionary); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkAllocationBaselineValidate(b *testing.B) {
	_, messageBytes, dictionary := allocationBaselineFIX40Order(b)
	message := NewMessage()
	if err := ParseMessageWithDataDictionary(message, bytes.NewBuffer(messageBytes), dictionary, dictionary); err != nil {
		b.Fatal(err)
	}
	validator := NewValidator(defaultValidatorSettings, dictionary, nil)
	if reject := validator.Validate(message); reject != nil {
		b.Fatal(reject)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if reject := validator.Validate(message); reject != nil {
			b.Fatal(reject)
		}
	}
}

func benchmarkAllocationBaselineBuild(b *testing.B) {
	message, messageBytes, _ := allocationBaselineFIX40Order(b)
	allocationBaselineBytes = message.build()

	b.ReportAllocs()
	b.SetBytes(int64(len(messageBytes)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		allocationBaselineBytes = message.build()
	}
}

func benchmarkAllocationBaselineReadFrame(b *testing.B) {
	_, messageBytes, _ := allocationBaselineFIX40Order(b)
	reader := &allocationBaselineRepeatingReader{message: messageBytes}
	parser := newParser(reader)
	frame, err := parser.ReadMessage()
	if err != nil {
		b.Fatal(err)
	}
	allocationBaselineFrame = frame

	b.ReportAllocs()
	b.SetBytes(int64(len(messageBytes)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frame, err = parser.ReadMessage()
		if err != nil {
			b.Fatal(err)
		}
		allocationBaselineFrame = frame
	}
}

func allocationBaselineFIX40Order(t testing.TB) (*Message, []byte, *datadictionary.DataDictionary) {
	t.Helper()

	dictionary, err := datadictionary.Parse("spec/FIX40.xml")
	if err != nil {
		t.Fatal(err)
	}
	message := createFIX40NewOrderSingle()
	return message, message.build(), dictionary
}

type allocationBaselineRepeatingReader struct {
	message []byte
	offset  int
}

func (r *allocationBaselineRepeatingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	n := copy(p, r.message[r.offset:])
	r.offset += n
	if r.offset == len(r.message) {
		r.offset = 0
	}
	return n, nil
}
