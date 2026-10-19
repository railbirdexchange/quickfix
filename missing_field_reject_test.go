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
			refTag := reject.RefTagID()
			if refTag == nil || *refTag != tag || reject.RefTagID() != refTag {
				test.Fatal("reference tag must have a stable, independently owned address")
			}
			other := ConditionallyRequiredFieldMissing(tag)
			if other.RefTagID() == refTag || reject == other {
				test.Fatal("independent rejects must not share mutable state or identity")
			}
			// The original implementation snapshots text before exposing RefTagID.
			*refTag = 7
			if *reject.RefTagID() != 7 || reject.Error() != wantText || *other.RefTagID() != tag || other.Error() != wantText {
				test.Fatal("reference-tag mutation changed text or another reject")
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
			if reject == nil || reject.Error() != "Conditionally Required Field Missing (41)" || *reject.RefTagID() != 41 {
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
				if reject.Error() != "Conditionally Required Field Missing (526)" || *reject.RefTagID() != 526 || reject.RejectReason() != 8 || !reject.IsBusinessReject() || reject.BusinessRejectRefID() != "" {
					test.Error("concurrent reject read changed")
					return
				}
			}
		}()
	}
	readers.Wait()
}
