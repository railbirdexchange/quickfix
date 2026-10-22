package quickfix

import "testing"

func TestFieldMapStringReadsPreserveValuesAndOwnership(test *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, getter := range []struct {
			name string
			read func(FieldMap, Tag) (string, MessageRejectError)
		}{{"public", FieldMap.GetString}, {"owner_locked", FieldMap.getStringNoLock}} {
			storage := "legacy"
			if fixed {
				storage = "fixed"
			}
			test.Run(storage+"/"+getter.name, func(test *testing.T) {
				var fields FieldMap
				if fixed {
					fields.initFixed(normalFieldOrder, 8)
				} else {
					fields.init()
				}
				for _, value := range [][]byte{nil, {}, {'0'}, []byte("RAILBIRD"), {0xff, 0, 0xfe}} {
					want := string(value)
					fields.SetBytes(tagMsgType, value)
					got, reject := getter.read(fields, tagMsgType)
					if reject != nil || got != want {
						test.Fatalf("value=%q got=%q reject=%v", want, got, reject)
					}
					for index := range value {
						value[index] = '?'
					}
					fields.Clear()
					fields.SetString(tagMsgType, "replacement")
					if got != want {
						test.Fatalf("retained string changed after byte/map reuse: %q, want %q", got, want)
					}
				}
				fields.Clear()
				got, reject := getter.read(fields, tagMsgType)
				wantReject := ConditionallyRequiredFieldMissing(tagMsgType)
				if reject == nil {
					test.Fatal("missing field did not return a reject")
				}
				refTagID, hasRefTagID := reject.RefTagID()
				if got != "" || reject == nil || reject.Error() != wantReject.Error() ||
					reject.RejectReason() != wantReject.RejectReason() || reject.IsBusinessReject() != wantReject.IsBusinessReject() ||
					!hasRefTagID || refTagID != tagMsgType || reject.BusinessRejectRefID() != wantReject.BusinessRejectRefID() {
					test.Fatalf("missing field changed: value=%q reject=%v", got, reject)
				}
			})
		}
	}
}
