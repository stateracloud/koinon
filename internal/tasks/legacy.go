// SPDX-License-Identifier: Apache-2.0

package tasks

// A version 0 payload is the free-form JSON object that writers stored before typed events. Most
// of its keys are already the proto field name or its JSON name of the member's message, so
// protojson reads them. A reshape moves the keys that differ; keys that the message does not have
// are dropped (the TaskEvent still carries the stored payload).

// reshape gives a version 0 object the shape of the member's message, in place.
type reshape func(obj map[string]any)

// same is the reshape for a payload whose keys need no change.
func same(map[string]any) {}

// renamed returns a reshape that moves each key old to its new name.
func renamed(names map[string]string) reshape {
	return func(obj map[string]any) {
		for old, name := range names {
			if v, ok := obj[old]; ok {
				delete(obj, old)
				obj[name] = v
			}
		}
	}
}

// scopeKeys maps the flat keys that approvalscope.Payload writes to the fields of ApprovalScope.
var scopeKeys = map[string]string{
	"scope_id":         "id",
	"scope_kind":       "kind",
	"scope_expires_at": "expires_at",
	"scope_created_by": "created_by",
	"scope_task_id":    "task_id",
}

// approvalScope nests the flat scope keys of an approval event in "scope".
func approvalScope(obj map[string]any) {
	scope := map[string]any{}
	for flat, name := range scopeKeys {
		if v, ok := obj[flat]; ok {
			delete(obj, flat)
			scope[name] = v
		}
	}
	if len(scope) > 0 {
		obj["scope"] = scope
	}
}
