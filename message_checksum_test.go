package quickfix

import (
	"fmt"
	"testing"
)

func TestMessageChecksumFormatting(t *testing.T) {
	var retained [256]string
	for value := range retained {
		retained[value] = formatCheckSum(value)
		if want := fmt.Sprintf("%03d", value); retained[value] != want {
			t.Fatalf("checksum %d = %q, want %q", value, retained[value], want)
		}
	}
	for _, fixture := range []struct {
		value int
		want  string
	}{
		{-1, "-01"}, {-12, "-12"}, {-123, "-123"}, {-1000, "-1000"},
		{256, "256"}, {999, "999"}, {1000, "1000"},
	} {
		if got := formatCheckSum(fixture.value); got != fixture.want {
			t.Fatalf("fallback checksum %d = %q, want %q", fixture.value, got, fixture.want)
		}
	}
	for value := len(retained) - 1; value >= 0; value-- {
		if got := formatCheckSum(value); got != retained[value] {
			t.Fatalf("checksum %d changed after further formatting", value)
		}
	}
}

func TestMessageChecksumSetterKeepsOwnedValues(t *testing.T) {
	for _, arena := range []bool{false, true} {
		message := newMessageStringFixture(t, "new_order_single")
		if arena {
			if err := message.attachMessageArena(4096, 64); err != nil {
				t.Fatal(err)
			}
		}
		retained := formatCheckSum(72)
		message.Trailer.SetString(tagCheckSum, retained)
		value, reject := message.Trailer.GetBytes(tagCheckSum)
		if reject != nil {
			t.Fatal(reject)
		}
		value[0] = '9'
		if retained != "072" || formatCheckSum(72) != "072" {
			t.Fatalf("arena=%v: mutable checksum getter changed retained formatting", arena)
		}
		message.Trailer.SetString(tagCheckSum, formatCheckSum(255))
		if retained != "072" {
			t.Fatalf("arena=%v: replacement changed retained checksum", arena)
		}
	}
}
