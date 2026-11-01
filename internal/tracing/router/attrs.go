// SPDX-License-Identifier: Apache-2.0

package router

import (
	"slices"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

func strVal(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func kvStr(k, v string) *commonpb.KeyValue { return &commonpb.KeyValue{Key: k, Value: strVal(v)} }

func kvInt(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func kvDouble(k string, v float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v}}}
}

func kvBool(k string, v bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}}}
}

// attrString returns the string attribute k.
func attrString(attrs []*commonpb.KeyValue, k string) (string, bool) {
	for _, kv := range attrs {
		if kv.GetKey() == k {
			if s, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue); ok {
				return s.StringValue, true
			}
		}
	}
	return "", false
}

// setAttr replaces attribute kv.Key or appends it.
func setAttr(attrs []*commonpb.KeyValue, kv *commonpb.KeyValue) []*commonpb.KeyValue {
	for i, have := range attrs {
		if have.GetKey() == kv.Key {
			attrs[i] = kv
			return attrs
		}
	}
	return append(attrs, kv)
}

// delAttr removes attribute k.
func delAttr(attrs []*commonpb.KeyValue, k string) []*commonpb.KeyValue {
	return slices.DeleteFunc(attrs, func(kv *commonpb.KeyValue) bool { return kv.GetKey() == k })
}

// stringList returns the strings of an array attribute.
func stringList(v *commonpb.AnyValue) []string {
	arr := v.GetArrayValue()
	if arr == nil {
		return nil
	}
	var out []string
	for _, e := range arr.GetValues() {
		if s, ok := e.GetValue().(*commonpb.AnyValue_StringValue); ok {
			out = append(out, s.StringValue)
		}
	}
	return out
}
