package quickfix

import (
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

func TestSessionValidationScratchReuse(t *testing.T) {
	for _, capacity := range []int{1, 3, 17, 512} {
		var seen validationTagSet
		seen.initFixed(capacity)
		for iteration := 0; iteration < 3; iteration++ {
			for index := 0; index < capacity; index++ {
				// Include the separately tracked zero tag and change occupied
				// hash positions on each reuse, including negative tags.
				tag := Tag(index * (iteration + 1))
				if iteration == 1 {
					tag = -tag
				}
				if !seen.add(tag) || seen.add(tag) {
					t.Fatalf("capacity %d, iteration %d: first/duplicate tag %d", capacity, iteration, tag)
				}
			}
			if seen.count != capacity || seen.overflow != nil {
				t.Fatalf("capacity %d: count=%d overflow=%v", capacity, seen.count, seen.overflow)
			}
			seen.reset()
			for _, tag := range seen.tags {
				if tag != 0 {
					t.Fatalf("reset retained tag %d", tag)
				}
			}
			if seen.count != 0 || seen.used != 0 || seen.hasZero {
				t.Fatal("reset retained occupancy metadata")
			}
		}
	}
}

func TestSessionValidatorLegacyOversizedDirectValidation(t *testing.T) {
	message, dictionary := inlineValidationWideMessage(t, validationInlineTagCapacity+1)
	validator, err := newSessionValidator(defaultValidatorSettings, dictionary, nil, 17)
	if err != nil {
		t.Fatal(err)
	}
	if reject := validator.Validate(message); reject != nil {
		t.Fatalf("direct legacy validation acquired a new size limit: %v", reject)
	}
	duplicate := inlineValidationDuplicate(t, message, message.fields[len(message.fields)-2].tag, 10000)
	assertBoundedRejectEqual(t, validator.Validate(duplicate), tagAppearsMoreThanOnce(10000))
	if reject := validator.Validate(message); reject != nil {
		t.Fatalf("legacy fallback retained duplicate state: %v", reject)
	}
	if validator.(*sessionValidator).bounded.seen.count != 0 {
		t.Fatal("oversized direct validation mutated fixed session scratch")
	}
}

func TestSessionValidatorRequiredRejectPrecedence(t *testing.T) {
	_, wire, dictionary := allocationBaselineFIX40Order(t)
	validator, err := newSessionValidator(defaultValidatorSettings, dictionary, nil, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name          string
		missingHeader bool
		missingBody   bool
		want          Tag
	}{
		{"header before body and trailer", true, true, tagSendingTime},
		{"body before trailer", false, true, 11},
		{"trailer", false, false, tagCheckSum},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			message := boundedValidationParse(t, wire)
			if fixture.missingHeader {
				message.Header.Remove(tagSendingTime)
			}
			if fixture.missingBody {
				message.Body.Remove(11)
			}
			message.Trailer.Remove(tagCheckSum)
			assertBoundedRejectEqual(t, validator.Validate(message), RequiredTagMissing(fixture.want))
			if reject := validator.Validate(boundedValidationParse(t, wire)); reject != nil {
				t.Fatalf("required-field failure leaked into valid input: %v", reject)
			}
		})
	}
}

func TestSessionValidatorFIXTRequiredDictionaries(t *testing.T) {
	transport, err := datadictionary.Parse("spec/FIXT11.xml")
	if err != nil {
		t.Fatal(err)
	}
	application, err := datadictionary.Parse("spec/FIX50.xml")
	if err != nil {
		t.Fatal(err)
	}
	validator, err := newSessionValidator(defaultValidatorSettings, application, transport, 64)
	if err != nil {
		t.Fatal(err)
	}
	legacy := NewValidator(defaultValidatorSettings, application, transport)
	for _, fixture := range []struct {
		messageType, body string
		missing           Tag
	}{
		{"A", "98=0\x01108=30\x011137=7\x01", 108},
		{"D", "11=order\x0121=1\x0138=1\x0140=1\x0154=1\x0155=SYM\x0160=20231231-20:19:41\x01", 11},
	} {
		t.Run(fixture.messageType, func(t *testing.T) {
			message := boundedValidationParse(t, buildFIXT11Wire("35="+fixture.messageType+"\x0134=1\x0149=S\x0156=T\x0152=20231231-20:19:41\x01", fixture.body))
			if reject := legacy.Validate(message); reject != nil {
				t.Fatalf("invalid legacy fixture: %v", reject)
			}
			if reject := validator.Validate(message); reject != nil {
				t.Fatalf("session required metadata selected wrong dictionary: %v", reject)
			}
			message.Body.Remove(fixture.missing)
			assertBoundedRejectEqual(t, validator.Validate(message), RequiredTagMissing(fixture.missing))
		})
	}
}

func TestSessionValidatorRequiredFixedLookup(t *testing.T) {
	_, wire, dictionary := allocationBaselineFIX40Order(t)
	limits := DefaultParseLimits()
	slot, err := NewMessageSlot(dictionary, dictionary, limits)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := newSessionValidator(defaultValidatorSettings, dictionary, nil, limits.MaxFields)
	if err != nil {
		t.Fatal(err)
	}
	for _, missing := range []Tag{tagSendingTime, 11, tagCheckSum} {
		if err := slot.Parse(wire); err != nil {
			t.Fatal(err)
		}
		message := slot.Message()
		if reject := validator.Validate(message); reject != nil {
			t.Fatalf("valid fixed fields rejected after reuse: %v", reject)
		}
		switch missing {
		case tagSendingTime:
			message.Header.Remove(missing)
		case tagCheckSum:
			message.Trailer.Remove(missing)
		default:
			message.Body.Remove(missing)
		}
		assertBoundedRejectEqual(t, validator.Validate(message), RequiredTagMissing(missing))
	}
}
