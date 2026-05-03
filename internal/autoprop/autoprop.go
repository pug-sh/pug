package autoprop

import (
	"strconv"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
)

const (
	PropBotScore     = "$bot_score"
	PropVerifiedBot  = "$verified_bot"
	PropLatitude     = "$latitude"
	PropLongitude    = "$longitude"
	PropScreenWidth  = "$screenWidth"
	PropScreenHeight = "$screenHeight"
	PropMobile       = "$mobile"
)

func PropertyValue(key, value string) *commonv1.PropertyValue {
	switch key {
	case PropVerifiedBot, PropMobile:
		if b, err := strconv.ParseBool(value); err == nil {
			return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_BoolValue{BoolValue: b}}
		}
	case PropBotScore, PropScreenWidth, PropScreenHeight:
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: n}}
		}
	case PropLatitude, PropLongitude:
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_DoubleValue{DoubleValue: f}}
		}
	}

	return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_StringValue{StringValue: value}}
}

func Variant(key, value string) chcol.Variant {
	pv := PropertyValue(key, value)
	switch v := pv.GetValue().(type) {
	case *commonv1.PropertyValue_StringValue:
		return chcol.NewVariantWithType(v.StringValue, "String")
	case *commonv1.PropertyValue_IntValue:
		return chcol.NewVariantWithType(v.IntValue, "Int64")
	case *commonv1.PropertyValue_DoubleValue:
		return chcol.NewVariantWithType(v.DoubleValue, "Float64")
	case *commonv1.PropertyValue_BoolValue:
		return chcol.NewVariantWithType(v.BoolValue, "Bool")
	default:
		return chcol.NewVariantWithType(value, "String")
	}
}
