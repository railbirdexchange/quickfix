package quickfix

import (
	"fmt"
	"sync"
	"testing"
)

func TestMissingFieldRejectPreservesContract(test *testing.T) {
	maxTag := Tag(int(^uint(0) >> 1))
	for _, tag := range []Tag{0, 1, 41, 255, 256, 526, 10001, -1, maxTag, -maxTag - 1} {
		test.Run(fmt.Sprint(tag), func(test *testing.T) {
			reject := ConditionallyRequiredFieldMissing(tag)
			wantText := fmt.Sprintf("Conditionally Required Field Missing (%d)", tag)
			if reject.Error() != wantText || reject.RejectReason() != 8 || !reject.IsBusinessReject() || reject.BusinessRejectRefID() != "" {
				test.Fatalf("incorrect missing-field reject: %v", reject)
			}
			for _, format := range []string{"%s", "%v", "%+v"} {
				if got := fmt.Sprintf(format, reject); got != wantText {
					test.Fatalf("format %s = %q, want %q", format, got, wantText)
				}
			}
			refTag, present := reject.RefTagID()
			if !present || refTag != tag {
				test.Fatalf("reference tag = (%d, %v), want (%d, true)", refTag, present, tag)
			}
			other := ConditionallyRequiredFieldMissing(tag)
			// Changing a returned reference value cannot alter either retained error.
			refTag = 7
			got, gotPresent := reject.RefTagID()
			otherTag, otherPresent := other.RefTagID()
			if !gotPresent || got != tag || !otherPresent || otherTag != tag || reject.Error() != wantText || other.Error() != wantText {
				test.Fatalf("changing returned value to %d changed a retained error", refTag)
			}
		})
	}
}

func TestMissingFieldRejectSurvivesMapReuse(test *testing.T) {
	for _, fixed := range []bool{false, true} {
		var fields FieldMap
		if fixed {
			fields.initFixed(normalFieldOrder, 8)
		} else {
			fields.init()
		}
		_, stringReject := fields.GetString(41)
		_, bytesReject := fields.GetBytes(41)
		var value FIXString
		fieldReject := fields.GetField(41, &value)
		groupReject := fields.GetGroup(NewRepeatingGroup(41, GroupTemplate{GroupElement(1)}))
		fields.SetString(41, "now-present")
		fields.Clear()
		fields.SetString(41, "replacement")
		for _, reject := range []MessageRejectError{stringReject, bytesReject, fieldReject, groupReject} {
			if reject == nil {
				test.Fatal("missing field did not return a reject")
			}
			tag, present := reject.RefTagID()
			if !present || tag != 41 || reject.Error() != "Conditionally Required Field Missing (41)" {
				test.Fatalf("retained reject changed after map reuse: %v", reject)
			}
		}
	}
}

func TestMissingFieldRejectConcurrentReads(test *testing.T) {
	reject := ConditionallyRequiredFieldMissing(526)
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				tag, present := reject.RefTagID()
				if reject.Error() != "Conditionally Required Field Missing (526)" || !present || tag != 526 || reject.RejectReason() != 8 || !reject.IsBusinessReject() || reject.BusinessRejectRefID() != "" {
					test.Error("concurrent reject read changed")
					return
				}
			}
		}()
	}
	readers.Wait()
}
