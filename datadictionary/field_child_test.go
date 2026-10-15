package datadictionary_test

import (
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

func TestGroupFieldChildNestedComponentsAndGroups(t *testing.T) {
	partyID := datadictionary.NewFieldDef(datadictionary.NewFieldType("PartyID", 448, "STRING"), true)
	partySource := datadictionary.NewFieldDef(datadictionary.NewFieldType("PartyIDSource", 447, "CHAR"), false)
	subID := datadictionary.NewFieldDef(datadictionary.NewFieldType("PartySubID", 523, "STRING"), true)
	subGroup := datadictionary.NewGroupFieldDef(
		datadictionary.NewFieldType("NoPartySubIDs", 802, "NUMINGROUP"), false,
		[]datadictionary.MessagePart{subID},
	)
	innerComponent := datadictionary.NewComponentType("InnerPartyDetails", []datadictionary.MessagePart{partySource, subGroup})
	outerComponent := datadictionary.NewComponentType("PartyDetails", []datadictionary.MessagePart{
		datadictionary.NewComponent(innerComponent, true),
	})
	group := datadictionary.NewGroupFieldDef(
		datadictionary.NewFieldType("NoPartyIDs", 453, "NUMINGROUP"), false,
		[]datadictionary.MessagePart{partyID, *datadictionary.NewComponent(outerComponent, true)},
	)

	for _, expected := range []*datadictionary.FieldDef{partyID, partySource, subGroup} {
		if got := group.Child(expected.Tag()); got != expected {
			t.Fatalf("Child(%d) = %p, want %p", expected.Tag(), got, expected)
		}
	}
	for _, absent := range []int{0, group.Tag(), subID.Tag(), 9999} {
		if got := group.Child(absent); got != nil {
			t.Fatalf("Child(%d) = %p, want nil", absent, got)
		}
	}
	if got := group.Child(subGroup.Tag()).Child(subID.Tag()); got != subID {
		t.Fatalf("nested Child(%d) = %p, want %p", subID.Tag(), got, subID)
	}
}

func TestFieldChildConstructorAndHandBuiltCompatibility(t *testing.T) {
	first := datadictionary.NewFieldDef(datadictionary.NewFieldType("First", 55, "STRING"), false)
	last := datadictionary.NewFieldDef(datadictionary.NewFieldType("Last", 55, "STRING"), true)
	nested := datadictionary.NewFieldDef(datadictionary.NewFieldType("Nested", 523, "STRING"), false)
	subGroup := datadictionary.NewGroupFieldDef(
		datadictionary.NewFieldType("NoNested", 802, "NUMINGROUP"), false,
		[]datadictionary.MessagePart{nested},
	)
	groupType := datadictionary.NewFieldType("NoEntries", 453, "NUMINGROUP")
	cases := []struct {
		name  string
		field *datadictionary.FieldDef
	}{
		{
			name: "constructed cache",
			field: datadictionary.NewGroupFieldDef(groupType, false,
				[]datadictionary.MessagePart{first, subGroup, last}),
		},
		{
			name: "hand built fallback",
			field: &datadictionary.FieldDef{
				FieldType: groupType,
				Fields:    []*datadictionary.FieldDef{first, subGroup, last},
			},
		},
	}
	for _, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			if got := fixture.field.Child(first.Tag()); got != last {
				t.Fatalf("duplicate Child(%d) = %p, want last definition %p", first.Tag(), got, last)
			}
			if got := fixture.field.Child(subGroup.Tag()); got != subGroup {
				t.Fatalf("Child(%d) = %p, want %p", subGroup.Tag(), got, subGroup)
			}
			if got := fixture.field.Child(nested.Tag()); got != nil {
				t.Fatalf("descendant Child(%d) = %p, want nil", nested.Tag(), got)
			}
			if got := fixture.field.Child(9999); got != nil {
				t.Fatalf("missing Child(9999) = %p, want nil", got)
			}
			allocations := testing.AllocsPerRun(100, func() {
				if fixture.field.Child(first.Tag()) != last || fixture.field.Child(9999) != nil {
					t.Fatal("child lookup changed during repeated reads")
				}
			})
			if allocations != 0 {
				t.Fatalf("Child allocated %g objects per hit/miss pair, want 0", allocations)
			}
		})
	}
	for _, field := range []*datadictionary.FieldDef{
		datadictionary.NewFieldDef(groupType, false),
		datadictionary.NewGroupFieldDef(groupType, false, nil),
		{},
	} {
		if got := field.Child(55); got != nil {
			t.Fatalf("empty Child(55) = %p, want nil", got)
		}
	}
}
