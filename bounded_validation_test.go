package quickfix

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

type boundedRejectSnapshot struct {
	text, businessRef string
	reason            int
	refTag            Tag
	present, hasTag   bool
	business          bool
}

func snapshotBoundedReject(reject MessageRejectError) boundedRejectSnapshot {
	if reject == nil {
		return boundedRejectSnapshot{}
	}
	snapshot := boundedRejectSnapshot{
		text: reject.Error(), businessRef: reject.BusinessRejectRefID(), reason: reject.RejectReason(),
		present: true, business: reject.IsBusinessReject(),
	}
	snapshot.refTag, snapshot.hasTag = reject.RefTagID()
	return snapshot
}

func assertBoundedRejectEqual(t testing.TB, actual, expected MessageRejectError) {
	t.Helper()
	if got, want := snapshotBoundedReject(actual), snapshotBoundedReject(expected); got != want {
		t.Fatalf("reject differs: got %#v, want %#v", got, want)
	}
}

func boundedValidationFromLegacy(t testing.TB, validator Validator, capacity int) *BoundedValidator {
	t.Helper()
	var bounded *BoundedValidator
	var err error
	switch legacy := validator.(type) {
	case *fixValidator:
		bounded, err = NewBoundedValidator(legacy.settings, legacy.dataDictionary, nil, capacity)
	case *fixtValidator:
		bounded, err = NewBoundedValidator(legacy.settings, legacy.appDataDictionary, legacy.transportDataDictionary, capacity)
	default:
		t.Fatalf("unsupported fixture validator %T", validator)
	}
	if err != nil {
		t.Fatal(err)
	}
	return bounded
}

func boundedValidationParse(t testing.TB, wire []byte) *Message {
	t.Helper()
	message := NewMessage()
	if err := ParseMessage(message, bytes.NewBuffer(wire)); err != nil {
		t.Fatal(err)
	}
	return message
}

func TestBoundedValidationRejectCompatibility(t *testing.T) {
	// Existing fixtures cover each reject category without copying their wires.
	for _, fixture := range []validateTest{
		tcInvalidTagNumberBody(), tcInvalidTagNumberHeaderFixT(),
		tcTagSpecifiedWithoutAValue(), tcTagSpecifiedWithoutAValueValidateHasValuesNoDataDictionary(),
		tcInvalidMsgType(), tcInvalidMsgTypeFixT(), tcValueIsIncorrect(),
		tcIncorrectDataFormatForValueFixT(), tcTagSpecifiedOutOfRequiredOrderHeader(),
		tcTagSpecifiedOutOfRequiredOrderTrailerFixT(), tcTagAppearsMoreThanOnce(),
		tcTagSpecifiedOutOfRequiredOrderDisabledHeader(), tcTagSpecifiedOutOfRequiredOrderDisabledHeaderFixT(),
		tcTagAppearsMoreThanOnceFixT(), tcTagNotDefinedForMessage(), tcFieldNotFoundBody(),
		tcMultipleRepeatingGroupFields(), tcMultiValueEnumValid(), tcMultiValueEnumInvalidComponent(),
	} {
		t.Run(fixture.TestName, func(t *testing.T) {
			message := boundedValidationParse(t, fixture.MessageBytes)
			bounded := boundedValidationFromLegacy(t, fixture.Validator, len(message.fields))
			sessionOwned, err := newSessionValidator(bounded.settings, bounded.application, bounded.transport, len(message.fields))
			if err != nil {
				t.Fatal(err)
			}
			want := fixture.Validator.Validate(message)
			if (want == nil) != fixture.DoNotExpectReject {
				t.Fatalf("legacy fixture outcome changed: %v", want)
			}
			if want != nil && want.RejectReason() != fixture.ExpectedRejectReason {
				t.Fatalf("legacy fixture reason = %d, want %d", want.RejectReason(), fixture.ExpectedRejectReason)
			}
			for iteration := 0; iteration < 3; iteration++ {
				got, err := bounded.Validate(message)
				if err != nil {
					t.Fatalf("validation operational error: %v", err)
				}
				assertBoundedRejectEqual(t, got, want)
				assertBoundedRejectEqual(t, sessionOwned.Validate(message), want)
			}
		})
	}
	bounded, err := NewBoundedValidator(defaultValidatorSettings, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	reject, err := bounded.Validate(NewMessage())
	if err != nil {
		t.Fatal(err)
	}
	assertBoundedRejectEqual(t, reject, RequiredTagMissing(tagMsgType))
}

func TestBoundedValidationPairedFaultPrecedence(t *testing.T) {
	dictionary, err := datadictionary.Parse("spec/FIX43.xml")
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name   string
		mutate func(*Message)
		want   MessageRejectError
	}{
		{"required before enum", func(m *Message) { m.Body.Remove(11); m.Body.SetString(18, "Y ZZ") }, RequiredTagMissing(11)},
		{"empty before enum", func(m *Message) { m.Body.SetString(55, ""); m.Body.SetString(18, "Y ZZ") }, TagSpecifiedWithoutAValue(55)},
		{"multi enum before scalar", func(m *Message) { m.Body.SetString(18, "Y ZZ"); m.Body.SetString(38, "bad") }, ValueIsIncorrect(18)},
		{"empty enum token before scalar", func(m *Message) { m.Body.SetString(18, "Y  M"); m.Body.SetString(38, "bad") }, ValueIsIncorrect(18)},
		{"timestamp before scalar", func(m *Message) { m.Header.SetString(52, "bad"); m.Body.SetString(38, "bad") }, IncorrectDataFormatForValue(52)},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			builder := createFIX43NewOrderSingle()
			fixture.mutate(builder)
			message := boundedValidationParse(t, builder.build())
			legacy := NewValidator(defaultValidatorSettings, dictionary, nil)
			assertBoundedRejectEqual(t, legacy.Validate(message), fixture.want)
			bounded := boundedValidationFromLegacy(t, legacy, len(message.fields))
			reject, err := bounded.Validate(message)
			if err != nil {
				t.Fatal(err)
			}
			assertBoundedRejectEqual(t, reject, fixture.want)
		})
	}
}

func TestBoundedValidationSettingsAndMultiEnums(t *testing.T) {
	dictionary, err := datadictionary.Parse("spec/FIX43.xml")
	if err != nil {
		t.Fatal(err)
	}
	for flags := 0; flags < 32; flags++ {
		settings := ValidatorSettings{
			CheckFieldsOutOfOrder: flags&1 != 0, RejectInvalidMessage: flags&2 != 0,
			AllowUnknownMessageFields: flags&4 != 0, CheckUserDefinedFields: flags&8 != 0,
			CheckFieldsHaveValues: flags&16 != 0,
		}
		t.Run(strconv.Itoa(flags), func(t *testing.T) {
			legacy := NewValidator(settings, dictionary, nil)
			bounded := boundedValidationFromLegacy(t, legacy, 64)
			sessionOwned, err := newSessionValidator(settings, dictionary, nil, 64)
			if err != nil {
				t.Fatal(err)
			}
			for _, fixture := range []struct {
				enum, unknown, empty string
			}{
				{"Y M", "", "A"}, {"Y ZZ", "", "A"}, {"Y  M", "", "A"},
				{" Y", "", "A"}, {"Y ", "", "A"}, {"Y M", "unknown", "A"},
				{"Y ZZ", "unknown", ""},
			} {
				builder := createFIX43NewOrderSingle()
				builder.Body.SetString(18, fixture.enum)
				builder.Body.SetString(55, fixture.empty)
				if fixture.unknown != "" {
					builder.Body.SetString(4999, fixture.unknown)
					builder.Body.SetString(9999, fixture.unknown)
				}
				message := boundedValidationParse(t, builder.build())
				reject, err := bounded.Validate(message)
				if err != nil {
					t.Fatal(err)
				}
				assertBoundedRejectEqual(t, reject, legacy.Validate(message))
				assertBoundedRejectEqual(t, sessionOwned.Validate(message), reject)
			}
		})
	}
}

func boundedValidationNestedDictionary() *datadictionary.DataDictionary {
	root := borrowedTestDefinition()
	field := func(name string, tag int, kind string) *datadictionary.FieldDef {
		return datadictionary.NewFieldDef(datadictionary.NewFieldType(name, tag, kind), true)
	}
	header := datadictionary.NewMessageDef("header", "", []datadictionary.MessagePart{
		field("BeginString", 8, "STRING"), field("BodyLength", 9, "LENGTH"), field("MsgType", 35, "STRING"),
	})
	trailer := datadictionary.NewMessageDef("trailer", "", []datadictionary.MessagePart{field("CheckSum", 10, "STRING")})
	dictionary := &datadictionary.DataDictionary{
		Header: header, Trailer: trailer,
		Messages:       map[string]*datadictionary.MessageDef{"D": datadictionary.NewMessageDef("Nested", "D", []datadictionary.MessagePart{root})},
		FieldTypeByTag: make(map[int]*datadictionary.FieldType),
	}
	for _, definition := range header.Fields {
		dictionary.FieldTypeByTag[definition.Tag()] = definition.FieldType
	}
	for _, definition := range trailer.Fields {
		dictionary.FieldTypeByTag[definition.Tag()] = definition.FieldType
	}
	var addFields func(*datadictionary.FieldDef)
	addFields = func(definition *datadictionary.FieldDef) {
		dictionary.FieldTypeByTag[definition.Tag()] = definition.FieldType
		for _, child := range definition.Fields {
			addFields(child)
		}
	}
	addFields(root)
	return dictionary
}

func boundedValidationNestedWire(body string) []byte {
	content := "35=D\x01" + body
	wire := []byte("8=FIX.4.4\x019=" + strconv.Itoa(len(content)) + "\x01" + content)
	return append(wire, []byte("10="+formatCheckSum(bytesTotal(wire)%256)+"\x01")...)
}

func TestBoundedValidationNestedReuse(t *testing.T) {
	dictionary := boundedValidationNestedDictionary()
	legacy := NewValidator(defaultValidatorSettings, dictionary, nil)
	bounded := boundedValidationFromLegacy(t, legacy, 64)
	slot, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		borrowedNestedWire,
		strings.Replace(borrowedNestedWire, "1000=2", "1000=3", 1),
		borrowedNestedWire,
		strings.Replace(borrowedNestedWire, "2000=2", "2000=3", 1),
		borrowedNestedWire + "1000=0\x01",
		"1000=0\x01",
		borrowedNestedWire,
	} {
		if err := slot.Parse(boundedValidationNestedWire(body)); err != nil {
			t.Fatal(err)
		}
		want := legacy.Validate(slot.Message())
		if body == borrowedNestedWire && want != nil {
			t.Fatalf("valid nested fixture rejected: %v", want)
		}
		reject, err := bounded.Validate(slot.Message())
		if err != nil {
			t.Fatal(err)
		}
		assertBoundedRejectEqual(t, reject, want)
	}
}

func TestBoundedValidationCapacityAndRecovery(t *testing.T) {
	_, wire, dictionary := allocationBaselineFIX40Order(t)
	message := boundedValidationParse(t, wire)
	capacity := len(message.fields)
	bounded, err := NewBoundedValidator(defaultValidatorSettings, dictionary, nil, capacity)
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 3; iteration++ {
		reject, err := bounded.Validate(message)
		if err != nil || reject != nil {
			t.Fatalf("exact capacity: reject = %v, error = %v", reject, err)
		}
		// The maps are deliberately uninitialized. The capacity check must run
		// before any validation traversal or access to the message header.
		oversized := &Message{fields: make([]TagValue, capacity+1)}
		if reject, err := bounded.Validate(oversized); err != ErrValidationCapacity || reject != nil {
			t.Fatalf("capacity overflow: reject = %v, error = %v", reject, err)
		}
	}
	if reject, err := bounded.Validate(message); reject != nil || err != nil {
		t.Fatalf("recovery after capacity failure: reject = %v, error = %v", reject, err)
	}
	for _, capacity := range []int{-1, 0, int(^uint(0) >> 1)} {
		validator, err := NewBoundedValidator(defaultValidatorSettings, dictionary, nil, capacity)
		if err != ErrInvalidValidationLimits || validator != nil {
			t.Fatalf("invalid capacity %d: validator = %v, error = %v", capacity, validator, err)
		}
	}
}

func TestLegacyValidatorConcurrentIndependentMessages(t *testing.T) {
	_, wire, dictionary := allocationBaselineFIX40Order(t)
	validator := NewValidator(defaultValidatorSettings, dictionary, nil)
	const workers = 8
	results := make(chan int, workers)
	for worker := 0; worker < workers; worker++ {
		message := boundedValidationParse(t, wire)
		if worker%2 != 0 {
			message.Body.SetString(38, "bad")
		}
		expected := snapshotBoundedReject(validator.Validate(message))
		go func(worker int, message *Message, expected boundedRejectSnapshot) {
			for iteration := 0; iteration < 100; iteration++ {
				if snapshotBoundedReject(validator.Validate(message)) != expected {
					results <- worker
					return
				}
			}
			results <- -1
		}(worker, message, expected)
	}
	for worker := 0; worker < workers; worker++ {
		if failed := <-results; failed >= 0 {
			t.Errorf("shared legacy validator changed worker %d's result", failed)
		}
	}
}

func boundedValidationSuccessFixture(t testing.TB, name string) (*Message, *BoundedValidator) {
	t.Helper()
	var dictionary *datadictionary.DataDictionary
	var wire []byte
	if name == "nested_groups" {
		dictionary = boundedValidationNestedDictionary()
		wire = boundedValidationNestedWire(borrowedNestedWire)
	} else {
		_, wire, dictionary = allocationBaselineFIX40Order(t)
	}
	slot, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := slot.Parse(wire); err != nil {
		t.Fatal(err)
	}
	validator, err := NewBoundedValidator(defaultValidatorSettings, dictionary, nil, len(slot.Message().fields))
	if err != nil {
		t.Fatal(err)
	}
	if reject, err := validator.Validate(slot.Message()); reject != nil || err != nil {
		t.Fatalf("warmup: reject = %v, error = %v", reject, err)
	}
	return slot.Message(), validator
}

func TestBoundedValidationAllocations(t *testing.T) {
	for _, name := range []string{"new_order_single", "nested_groups"} {
		t.Run(name, func(t *testing.T) {
			message, validator := boundedValidationSuccessFixture(t, name)
			allocations := testing.AllocsPerRun(100, func() {
				if reject, err := validator.Validate(message); reject != nil || err != nil {
					t.Fatalf("reject = %v, error = %v", reject, err)
				}
			})
			if allocations != 0 {
				t.Fatalf("successful validation allocated %g objects, want 0", allocations)
			}
		})
	}
}

func BenchmarkBoundedValidation(b *testing.B) {
	for _, name := range []string{"new_order_single", "nested_groups"} {
		b.Run(name, func(b *testing.B) {
			message, validator := boundedValidationSuccessFixture(b, name)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if reject, err := validator.Validate(message); reject != nil || err != nil {
					b.Fatalf("reject = %v, error = %v", reject, err)
				}
			}
		})
	}
}
