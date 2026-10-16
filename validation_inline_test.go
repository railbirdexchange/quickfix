package quickfix

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

// These fixtures use the ordinary public parser and validator. No slot,
// bounded validator, or caller-provided scratch selects the allocation path.
func inlineValidationWideMessage(t testing.TB, fields int) (*Message, *datadictionary.DataDictionary) {
	t.Helper()
	source, wire, dictionary := allocationBaselineFIX40Order(t)
	base := boundedValidationParse(t, wire)
	for index := len(base.fields); index < fields; index++ {
		tag := 10000 + index - len(base.fields)
		definition := datadictionary.NewFieldType("Extra"+strconv.Itoa(tag), tag, "STRING")
		dictionary.FieldTypeByTag[tag] = definition
		dictionary.Messages["D"].Fields[tag] = datadictionary.NewFieldDef(definition, false)
		source.Body.SetString(Tag(tag), "value")
	}
	message := boundedValidationParse(t, source.build())
	if len(message.fields) != fields {
		t.Fatalf("fixture has %d fields, want %d", len(message.fields), fields)
	}
	return message, dictionary
}

func inlineValidationDuplicate(t testing.TB, message *Message, replace, duplicate Tag) *Message {
	t.Helper()
	wire := bytes.Clone(message.rawMessage.Bytes())
	oldTag, newTag := strconv.Itoa(int(replace))+"=", strconv.Itoa(int(duplicate))+"="
	if len(oldTag) != len(newTag) {
		t.Fatal("fixture replacement changes BodyLength")
	}
	index := bytes.Index(wire, []byte("\x01"+oldTag))
	if index < 0 {
		t.Fatalf("fixture does not contain tag %d", replace)
	}
	copy(wire[index+1:], newTag)
	checksum := bytesTotal(wire[:len(wire)-7]) % 256
	copy(wire[len(wire)-4:], formatCheckSum(checksum))
	return boundedValidationParse(t, wire)
}

func TestValidatorInlineUnlimitedInput(t *testing.T) {
	for _, fields := range []int{validationInlineTagCapacity, validationInlineTagCapacity + 1, 2049} {
		t.Run(strconv.Itoa(fields), func(t *testing.T) {
			message, dictionary := inlineValidationWideMessage(t, fields)
			validator := NewValidator(defaultValidatorSettings, dictionary, nil)
			if reject := validator.Validate(message); reject != nil {
				t.Fatalf("valid legacy message was rejected: %v", reject)
			}
			last := message.fields[len(message.fields)-2].tag
			// A duplicate of an early tag must remain visible after migration
			// from inline storage to the unlimited private map.
			duplicate := inlineValidationDuplicate(t, message, last, 10000)
			assertBoundedRejectEqual(t, validator.Validate(duplicate), tagAppearsMoreThanOnce(10000))
			if fields > 1024 {
				// Also detect duplicates first introduced after the inline limit.
				duplicate = inlineValidationDuplicate(t, message, last, 11000)
				assertBoundedRejectEqual(t, validator.Validate(duplicate), tagAppearsMoreThanOnce(11000))
			}
			if reject := validator.Validate(message); reject != nil {
				t.Fatalf("a prior duplicate leaked into another validation call: %v", reject)
			}
		})
	}
}

func TestValidatorInlineTagDomainAndSpill(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, zeroFirst := range []bool{false, true} {
		var seen validationTagSet
		initial := []Tag{Tag(-maximum - 1), Tag(maximum), -1}
		if zeroFirst {
			initial = append(initial, 0)
		}
		for _, tag := range initial {
			if !seen.add(tag) || seen.add(tag) {
				t.Fatalf("tag %d lost its first/duplicate distinction", tag)
			}
		}
		for tag := Tag(1); seen.count < validationInlineTagCapacity; tag++ {
			if !seen.add(tag) {
				t.Fatalf("unexpected inline duplicate %d", tag)
			}
		}
		if seen.overflow != nil {
			t.Fatal("exact inline capacity spilled")
		}
		last := Tag(0)
		if zeroFirst {
			last = -2
		}
		if !seen.add(last) || seen.overflow == nil || seen.add(last) {
			t.Fatal("overflow did not retain its triggering tag")
		}
		for _, tag := range initial {
			if seen.add(tag) {
				t.Fatalf("tag %d disappeared during overflow", tag)
			}
		}
	}
}

func TestValidatorInlineSharedImmutableMessages(t *testing.T) {
	message, dictionary := inlineValidationWideMessage(t, 700)
	duplicate := inlineValidationDuplicate(t, message, message.fields[len(message.fields)-2].tag, 10000)
	validator := NewValidator(defaultValidatorSettings, dictionary, nil)
	want := snapshotBoundedReject(tagAppearsMoreThanOnce(10000))
	const workers = 8
	results := make(chan int, workers)
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			for iteration := 0; iteration < 100; iteration++ {
				if validator.Validate(message) != nil || snapshotBoundedReject(validator.Validate(duplicate)) != want {
					results <- worker
					return
				}
			}
			results <- -1
		}(worker)
	}
	for range workers {
		if worker := <-results; worker >= 0 {
			t.Errorf("shared immutable messages changed worker %d's result", worker)
		}
	}
}

func TestValidatorInlineOrdinaryAPIsAllocateZero(t *testing.T) {
	for _, name := range []string{"new_order_single", "nested_groups", "512_fields"} {
		t.Run(name, func(t *testing.T) {
			var message *Message
			var dictionary *datadictionary.DataDictionary
			switch name {
			case "512_fields":
				message, dictionary = inlineValidationWideMessage(t, validationInlineTagCapacity)
			case "nested_groups":
				dictionary = boundedValidationNestedDictionary()
				message = boundedValidationParse(t, boundedValidationNestedWire(borrowedNestedWire))
			default:
				var wire []byte
				_, wire, dictionary = allocationBaselineFIX40Order(t)
				message = boundedValidationParse(t, wire)
			}
			validator := NewValidator(defaultValidatorSettings, dictionary, nil)
			if reject := validator.Validate(message); reject != nil {
				t.Fatalf("warm-up rejected valid message: %v", reject)
			}
			var reject MessageRejectError
			allocations := testing.AllocsPerRun(100, func() { reject = validator.Validate(message) })
			if reject != nil || allocations != 0 {
				t.Fatalf("ordinary Validate: reject=%v allocations=%g, want nil and zero", reject, allocations)
			}
		})
	}
}
