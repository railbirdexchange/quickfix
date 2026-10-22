package quickfix

import (
	"testing"

	"github.com/shopspring/decimal"
)

var decimalReadResult decimal.Decimal

// decimalReadQuantityField and readDecimalQuantity retain the shape of the
// generated OrderQty field/getter without adding generated packages to this
// module's dependencies. The caller receives an owned shopspring Decimal.
type decimalReadQuantityField struct{ FIXDecimal }

func (decimalReadQuantityField) Tag() Tag { return 38 }

func readDecimalQuantity(message *Message) (decimal.Decimal, MessageRejectError) {
	var field decimalReadQuantityField
	if reject := message.Body.Get(&field); reject != nil {
		return decimal.Decimal{}, reject
	}
	return field.Decimal, nil
}

func BenchmarkFIXDecimalOwnedRead(benchmark *testing.B) {
	for _, fixture := range []struct {
		name  string
		value string
	}{
		{name: "quantity", value: "100"},
		{name: "price", value: "123.45"},
		{name: "signed_fraction", value: "-124.3456"},
		{name: "zero_scale", value: "-0.0000"},
		{name: "exponent", value: "1.2345e+2"},
		{name: "int64_max", value: "9223372036854775807"},
		{name: "int64_min", value: "-9223372036854775808"},
		{name: "overflow", value: "9223372036854775808.12345"},
	} {
		benchmark.Run(fixture.name, func(benchmark *testing.B) {
			input := []byte(fixture.value)
			benchmark.Run("read", func(benchmark *testing.B) {
				var field FIXDecimal
				if err := field.Read(input); err != nil {
					benchmark.Fatal(err)
				}
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				for iteration := 0; iteration < benchmark.N; iteration++ {
					if err := field.Read(input); err != nil {
						benchmark.Fatal(err)
					}
					decimalReadResult = field.Decimal
				}
			})
			benchmark.Run("field_getter", func(benchmark *testing.B) {
				message := NewMessage()
				message.Body.SetBytes(38, input)
				if _, reject := readDecimalQuantity(message); reject != nil {
					benchmark.Fatal(reject)
				}
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				for iteration := 0; iteration < benchmark.N; iteration++ {
					value, reject := readDecimalQuantity(message)
					if reject != nil {
						benchmark.Fatal(reject)
					}
					decimalReadResult = value
				}
			})
		})
	}
}
