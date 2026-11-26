// SPDX-License-Identifier: Apache-2.0

package tasks

import "google.golang.org/protobuf/reflect/protoreflect"

// CatalogEntry returns the catalog row of a oneof member for the suite: its kind and whether it
// has a version 0 reshape.
func CatalogEntry(member protoreflect.Name) (kind string, legacy, ok bool) {
	e, ok := catalog[member]
	return e.kind, e.legacy != nil, ok
}
