package quickfix

import (
	"bytes"
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

const borrowedNestedWire = "1000=2\x011001=first\x011002=outer\x012000=2\x012001=a\x012002=av\x014000=1\x014001=c\x014002=cv\x012001=b\x012002=bv\x014000=0\x013000=1\x013001=z\x013002=zv\x011003=end\x011001=second\x013000=0\x011003=done\x01"

func borrowedTestDefinition() *datadictionary.FieldDef {
	field := func(tag int) *datadictionary.FieldDef {
		return datadictionary.NewFieldDef(datadictionary.NewFieldType("field", tag, "STRING"), false)
	}
	group := func(tag int, parts ...datadictionary.MessagePart) *datadictionary.FieldDef {
		return datadictionary.NewGroupFieldDef(datadictionary.NewFieldType("group", tag, "NUMINGROUP"), false, parts)
	}
	deep := group(4000, field(4001), field(4002))
	first := group(2000, field(2001), field(2002), deep)
	second := group(3000, field(3001), field(3002))
	return group(1000, field(1001), field(1002), first, second, field(1003))
}

func borrowedTestValues(t *testing.T, wire string) []TagValue {
	t.Helper()
	input := []byte(wire)
	values := make([]TagValue, bytes.Count(input, []byte{'\x01'}))
	remaining := input
	for index := range values {
		var err error
		remaining, err = extractField(&values[index], remaining)
		if err != nil {
			t.Fatal(err)
		}
	}
	return values
}

func TestBorrowedGroupNestedSiblingViews(t *testing.T) {
	group, err := newBorrowedGroup(borrowedTestValues(t, borrowedNestedWire), borrowedTestDefinition())
	if err != nil {
		t.Fatal(err)
	}
	if !borrowedTestTraversal(group) {
		t.Fatal("nested sibling traversal returned incorrect fields or boundaries")
	}
	iterator := group.Iter()
	first, ok := iterator.Next()
	if !ok {
		t.Fatal("missing first entry")
	}
	for _, missing := range []Tag{1000, 2001, 4001, 9999} {
		if value, found := first.GetBytes(missing); found || value != nil {
			t.Fatalf("GetBytes(%d) returned a non-immediate field", missing)
		}
	}
	for _, missing := range []Tag{1001, 4000, 9999} {
		if _, err := first.Group(missing); err != ErrBorrowedGroupNotFound {
			t.Fatalf("Group(%d) error = %v, want ErrBorrowedGroupNotFound", missing, err)
		}
	}
	if count, found := first.GetBytes(2000); !found || string(count) != "2" {
		t.Fatalf("GetBytes on nested count = %q, %v; want 2, true", count, found)
	}
}

func borrowedTestTraversal(group BorrowedGroup) bool {
	if group.Len() != 2 {
		return false
	}
	iterator := group.Iter()
	first, ok := iterator.Next()
	if !ok {
		return false
	}
	value, ok := first.GetBytes(1003)
	if !ok || string(value) != "end" {
		return false
	}
	children, err := first.Group(2000)
	if err != nil || children.Len() != 2 {
		return false
	}
	childIterator := children.Iter()
	child, ok := childIterator.Next()
	if !ok {
		return false
	}
	deep, err := child.Group(4000)
	if err != nil || deep.Len() != 1 {
		return false
	}
	deepIterator := deep.Iter()
	deepEntry, ok := deepIterator.Next()
	if !ok {
		return false
	}
	value, ok = deepEntry.GetBytes(4002)
	if !ok || string(value) != "cv" {
		return false
	}
	if _, ok := deepIterator.Next(); ok {
		return false
	}
	child, ok = childIterator.Next()
	if !ok {
		return false
	}
	value, ok = child.GetBytes(2001)
	if !ok || string(value) != "b" {
		return false
	}
	deep, err = child.Group(4000)
	if err != nil || deep.Len() != 0 {
		return false
	}
	deepIterator = deep.Iter()
	if _, ok := deepIterator.Next(); ok {
		return false
	}
	if _, ok := childIterator.Next(); ok {
		return false
	}
	siblings, err := first.Group(3000)
	if err != nil || siblings.Len() != 1 {
		return false
	}
	siblingIterator := siblings.Iter()
	sibling, ok := siblingIterator.Next()
	if !ok {
		return false
	}
	value, ok = sibling.GetBytes(3002)
	if !ok || string(value) != "zv" {
		return false
	}
	second, ok := iterator.Next()
	if !ok {
		return false
	}
	value, ok = second.GetBytes(1001)
	if !ok || string(value) != "second" {
		return false
	}
	siblings, err = second.Group(3000)
	if err != nil || siblings.Len() != 0 {
		return false
	}
	_, ok = iterator.Next()
	return !ok
}

func TestBorrowedGroupCountAndBoundaryErrors(t *testing.T) {
	definition := borrowedTestDefinition()
	for _, wire := range []string{
		"", "999=0\x01", "1000=\x01", "1000=-\x01", "1000=-1\x01", "1000=+1\x01",
		"1000=one\x01", "1000=999999999999999999999999999999999999\x01",
		"1000=1\x01", "1000=2\x011001=first\x01", "1000=1\x011002=missing-delimiter\x01",
		"1000=0\x011001=unexpected\x01", "1000=1\x011001=first\x011001=extra-entry\x01",
		"1000=1\x011001=first\x012000=1\x013000=0\x01",
		"1000=1\x011001=first\x012000=1\x012001=a\x012001=extra-nested-entry\x01",
		"1000=1\x011001=first\x012000=2\x012001=a\x013000=1\x013001=z\x01",
		"1000=1\x011001=first\x019999=unknown\x01",
	} {
		t.Run(wire, func(t *testing.T) {
			if _, err := newBorrowedGroup(borrowedTestValues(t, wire), definition); err != ErrInvalidBorrowedGroup {
				t.Fatalf("error = %v, want ErrInvalidBorrowedGroup", err)
			}
		})
	}
	for _, count := range []string{"0", "00", "-0"} {
		group, err := newBorrowedGroup(borrowedTestValues(t, "1000="+count+"\x01"), definition)
		if err != nil || group.Len() != 0 {
			t.Fatalf("zero group %q: length = %d, error = %v", count, group.Len(), err)
		}
		iterator := group.Iter()
		if _, ok := iterator.Next(); ok {
			t.Fatal("zero group yielded an entry")
		}
	}
	var empty BorrowedGroup
	iterator := empty.Iter()
	if empty.Len() != 0 {
		t.Fatal("zero-value group is nonempty")
	}
	if _, ok := iterator.Next(); ok {
		t.Fatal("zero-value iterator yielded an entry")
	}
}

func TestBorrowedGroupDoesNotReadBeyondFieldSlice(t *testing.T) {
	values := borrowedTestValues(t, "1000=1\x011001=first\x019999=adjacent\x01")
	group, err := newBorrowedGroup(values[:2], borrowedTestDefinition())
	if err != nil || group.Len() != 1 {
		t.Fatalf("bounded slice: length = %d, error = %v", group.Len(), err)
	}
	if _, err := newBorrowedGroup(values, borrowedTestDefinition()); err != ErrInvalidBorrowedGroup {
		t.Fatalf("group including adjacent field: error = %v", err)
	}
}

func TestBorrowedGroupFIX44Dictionary(t *testing.T) {
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	values := borrowedTestValues(t, "453=2\x01448=4501\x01447=D\x01452=28\x01448=4502\x01447=D\x01452=28\x01")
	group, err := newBorrowedGroup(values, dictionary.Messages["D"].Fields[453])
	if err != nil || group.Len() != 2 {
		t.Fatalf("FIX44 party group: length = %d, error = %v", group.Len(), err)
	}
	iterator := group.Iter()
	for _, expected := range []string{"4501", "4502"} {
		entry, ok := iterator.Next()
		if !ok {
			t.Fatal("missing party group entry")
		}
		value, ok := entry.GetBytes(448)
		if !ok || string(value) != expected {
			t.Fatalf("PartyID = %q, %v; want %q, true", value, ok, expected)
		}
	}
	if _, ok := iterator.Next(); ok {
		t.Fatal("unexpected party group entry")
	}
}

func TestBorrowedGroupReusesInputAndFieldStorage(t *testing.T) {
	definition := borrowedTestDefinition()
	storage := []byte("1000=1\x011001=first\x01")
	var values [2]TagValue
	remaining, err := extractField(&values[0], storage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extractField(&values[1], remaining); err != nil {
		t.Fatal(err)
	}
	group, err := newBorrowedGroup(values[:], definition)
	if err != nil {
		t.Fatal(err)
	}
	iterator := group.Iter()
	entry, ok := iterator.Next()
	if !ok {
		t.Fatal("missing entry")
	}
	value, ok := entry.GetBytes(1001)
	if !ok || &value[0] != &storage[len("1000=1\x011001=")] {
		t.Fatal("entry did not borrow its input value")
	}
	// End the first borrow before reusing the slot's input and field slab.
	group, iterator, entry, value = BorrowedGroup{}, BorrowedGroupIterator{}, BorrowedGroupEntry{}, nil
	copy(storage, "1000=1\x011001=other\x01")
	remaining, err = extractField(&values[0], storage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extractField(&values[1], remaining); err != nil {
		t.Fatal(err)
	}
	group, err = newBorrowedGroup(values[:], definition)
	if err != nil {
		t.Fatal(err)
	}
	iterator = group.Iter()
	entry, ok = iterator.Next()
	if !ok {
		t.Fatal("missing reused entry")
	}
	value, ok = entry.GetBytes(1001)
	if !ok || string(value) != "other" {
		t.Fatalf("reused value = %q, %v; want other, true", value, ok)
	}
}

func TestBorrowedGroupAllocations(t *testing.T) {
	definition := borrowedTestDefinition()
	values := borrowedTestValues(t, borrowedNestedWire)
	allocations := testing.AllocsPerRun(100, func() {
		group, err := newBorrowedGroup(values, definition)
		if err != nil || !borrowedTestTraversal(group) {
			t.Fatal("group construction or traversal failed")
		}
	})
	if allocations != 0 {
		t.Fatalf("construction and nested traversal allocated %g objects, want 0", allocations)
	}
	invalid := borrowedTestValues(t, "1000=1\x011001=first\x012000=-1\x01")
	allocations = testing.AllocsPerRun(100, func() {
		if _, err := newBorrowedGroup(invalid, definition); err != ErrInvalidBorrowedGroup {
			t.Fatal("invalid nested group did not fail")
		}
	})
	if allocations != 0 {
		t.Fatalf("invalid group allocated %g objects, want 0", allocations)
	}
}
