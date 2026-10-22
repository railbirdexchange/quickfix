package quickfix

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

func TestConfiguredValidationMetadataApplicabilityAndFallback(t *testing.T) {
	fixture := newConfiguredValidationFixture(t, configuredValidationScenario{name: "valid"})
	metadata := fixture.validator.(*sessionValidator).bounded.requiredApp
	t.Logf("FIX44 configured_pairs=%d compact_result_block_bytes=%d; lookup and required metadata additional",
		len(metadata.configured), uintptr(len(metadata.configured))*reflect.TypeOf(configuredValidationReject{}).Size())
	for _, item := range []struct {
		tag    Tag
		reason int
		exists bool
	}{
		{38, rejectReasonIncorrectDataFormatForValue, true},
		{11, rejectReasonIncorrectDataFormatForValue, false},
		{54, rejectReasonValueIsIncorrect, true},
		{11, rejectReasonValueIsIncorrect, false},
		{453, rejectReasonIncorrectNumInGroupCountForRepeatingGroup, true},
		{11, rejectReasonIncorrectNumInGroupCountForRepeatingGroup, false},
		{49, rejectReasonTagSpecifiedOutOfRequiredOrder, true},
		{11, rejectReasonTagAppearsMoreThanOnce, true},
		{11, rejectReasonTagSpecifiedWithoutAValue, true},
		{7, rejectReasonTagNotDefinedForThisMessageType, true},
	} {
		key := configuredValidationReject{tag: item.tag, reason: item.reason}
		if got := metadata.configured[key] != nil; got != item.exists {
			t.Fatalf("tag=%d reason=%d prepared=%v, want %v", item.tag, item.reason, got, item.exists)
		}
	}

	count := len(metadata.configured)
	for _, scenario := range configuredValidationScenarios() {
		if scenario.expected == nil || scenario.nested || scenario.name == "unknown_tag" {
			continue
		}
		tag, _ := scenario.expected.RefTagID()
		reason := scenario.expected.RejectReason()
		assertBoundedRejectEqual(t, metadata.reject(tag, reason), scenario.expected)
		assertBoundedRejectEqual(t, (*validationRequiredMetadata)(nil).reject(tag, reason), scenario.expected)
		for _, unknown := range []Tag{0, -1, 100001, Tag(int(^uint(0) >> 1)), Tag(-int(^uint(0)>>1) - 1)} {
			expected := NewMessageRejectError(scenario.expected.Error(), reason, &unknown)
			got := metadata.reject(unknown, reason)
			assertBoundedRejectEqual(t, got, expected)
			if _, owned := got.(messageRejectError); !owned {
				t.Fatalf("unknown tag %d lost the legacy owned constructor", unknown)
			}
		}
	}
	if len(metadata.configured) != count {
		t.Fatal("unknown lookup grew setup metadata")
	}

	for _, scenario := range configuredValidationScenarios() {
		if !scenario.nested {
			continue
		}
		nested := newConfiguredValidationFixture(t, scenario)
		nestedMetadata := nested.validator.(*sessionValidator).bounded.requiredApp
		if nestedMetadata.rejects[2002] == nil {
			t.Fatal("required nested group child was not prepared")
		}
	}
}

func TestConfiguredValidationFIXTGroupMetadata(t *testing.T) {
	field := func(number int, required bool) *datadictionary.FieldDef {
		return datadictionary.NewFieldDef(datadictionary.NewFieldType("fixture", number, "STRING"), required)
	}
	header := datadictionary.NewMessageDef("header", "", []datadictionary.MessagePart{
		field(8, true), field(9, true), field(35, true),
		datadictionary.NewGroupFieldDef(field(627, false).FieldType, false, []datadictionary.MessagePart{field(628, true), field(629, true)}),
	})
	trailer := datadictionary.NewMessageDef("trailer", "", []datadictionary.MessagePart{
		datadictionary.NewGroupFieldDef(field(93, false).FieldType, false, []datadictionary.MessagePart{field(89, true)}),
		field(10, true),
	})
	transport := &datadictionary.DataDictionary{
		Header: header, Trailer: trailer,
		Messages:       map[string]*datadictionary.MessageDef{"0": datadictionary.NewMessageDef("Heartbeat", "0", nil)},
		FieldTypeByTag: make(map[int]*datadictionary.FieldType),
	}
	application := &datadictionary.DataDictionary{
		Messages:       map[string]*datadictionary.MessageDef{"D": datadictionary.NewMessageDef("Application", "D", nil)},
		FieldTypeByTag: make(map[int]*datadictionary.FieldType),
	}
	for _, definition := range []*datadictionary.MessageDef{header, trailer} {
		for _, root := range definition.Fields {
			transport.FieldTypeByTag[root.Tag()] = root.FieldType
			for _, child := range root.Fields {
				transport.FieldTypeByTag[child.Tag()] = child.FieldType
			}
		}
	}
	for _, messageType := range []string{"D", "0"} {
		for _, scenario := range []struct {
			name, old, replacement string
			expected               MessageRejectError
		}{
			{name: "valid"},
			{name: "header_format", old: "627=1", replacement: "627=x", expected: IncorrectDataFormatForValue(627)},
			{name: "header_plus", old: "627=1", replacement: "627=+1", expected: IncorrectDataFormatForValue(627)},
			{name: "header_count", old: "627=1", replacement: "627=2", expected: incorrectNumInGroupCountForRepeatingGroup(627)},
			{name: "header_negative", old: "627=1", replacement: "627=-1", expected: incorrectNumInGroupCountForRepeatingGroup(627)},
			// Preserve the existing FIXInt overflow behavior; do not introduce a
			// stricter numeric parser while changing only error construction.
			{name: "header_overflow", old: "627=1", replacement: "627=18446744073709551617"},
			{name: "header_required", old: "629=now\x01", replacement: "", expected: RequiredTagMissing(629)},
			{name: "trailer_format", old: "93=1", replacement: "93=x", expected: IncorrectDataFormatForValue(93)},
			{name: "trailer_count", old: "93=1", replacement: "93=2", expected: incorrectNumInGroupCountForRepeatingGroup(93)},
			{name: "trailer_missing_delimiter", old: "89=signature\x01", replacement: "", expected: incorrectNumInGroupCountForRepeatingGroup(93)},
		} {
			t.Run(messageType+"/"+scenario.name, func(t *testing.T) {
				content := "35=" + messageType + "\x01627=1\x01628=hop\x01629=now\x0193=1\x0189=signature\x01"
				if scenario.old != "" {
					content = strings.Replace(content, scenario.old, scenario.replacement, 1)
				}
				wire := []byte("8=FIXT.1.1\x019=" + strconv.Itoa(len(content)) + "\x01" + content)
				wire = append(wire, []byte("10="+formatCheckSum(bytesTotal(wire)%256)+"\x01")...)
				message := boundedValidationParse(t, wire)
				validator, err := newSessionValidator(defaultValidatorSettings, application, transport, 64)
				if err != nil {
					t.Fatal(err)
				}
				assertBoundedRejectEqual(t, validator.Validate(message), scenario.expected)
				assertBoundedRejectEqual(t, NewValidator(defaultValidatorSettings, application, transport).Validate(message), scenario.expected)
				metadata := validator.(*sessionValidator).bounded.requiredApp
				if messageType == "0" {
					metadata = validator.(*sessionValidator).bounded.requiredAdmin
				}
				for _, root := range []Tag{627, 93} {
					if metadata.configured[configuredValidationReject{tag: root, reason: rejectReasonIncorrectDataFormatForValue}] == nil {
						t.Fatalf("custom STRING group %d lacked its integer-format reject", root)
					}
				}
			})
		}
	}
}

func TestConfiguredRequiredGroupCompletedReject(t *testing.T) {
	fixture := newScopedOutboundBenchmarkWithDelivery(t, true, "", true)
	defer fixture.close()
	scenario := configuredValidationScenario{
		name: "required_party_source", old: "447=D\x01", replacement: "",
		expected: RequiredTagMissing(447),
	}
	wire, _, dictionary := configuredValidationInput(t, scenario)
	parties := dictionary.Messages["D"].Fields[453]
	child := parties.Fields[1]
	if child.Tag() != 447 {
		t.Fatal("unexpected Parties fixture layout")
	}
	parties.Fields[1] = datadictionary.NewFieldDef(child.FieldType, true)
	validator, err := newSessionValidator(defaultValidatorSettings, dictionary, nil, DefaultParseLimits().MaxFields)
	if err != nil {
		t.Fatal(err)
	}
	fixture.session.Validator = validator
	if err := fixture.session.boundedIn.parsed.Parse(wire); err != nil {
		t.Fatal(err)
	}
	fixture.message = fixture.session.boundedIn.parsed.Message()
	assertBoundedRejectEqual(t, validator.Validate(fixture.message), scenario.expected)
	assertBoundedRejectEqual(t, NewValidator(defaultValidatorSettings, dictionary, nil).Validate(fixture.message), scenario.expected)
	application := &scopedControlApplication{}
	fixture.session.application = BorrowApplication(application)
	completed := configuredCompletedRejectFixture{scopedOutboundBenchmark: fixture, application: application, expected: scenario.expected}
	for range 3 {
		if err := completed.complete(); err != nil {
			t.Fatal(err)
		}
	}
}
