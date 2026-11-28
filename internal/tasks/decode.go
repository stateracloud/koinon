// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
)

// Payload versions of task_events.payload_version.
const (
	// VersionLegacy is the free-form JSON that writers stored before typed events.
	VersionLegacy int16 = 0
	// VersionTyped is the protojson form of one TimelineEvent.
	VersionTyped int16 = 1
)

var (
	// ErrUnknownKind is returned for a kind that the catalog does not name.
	ErrUnknownKind = errors.New("unknown timeline event kind")
	// ErrUnknownVersion is returned for a payload version that this reader does not know.
	ErrUnknownVersion = errors.New("unknown timeline payload version")
)

// read accepts fields that a newer writer added, so an older reader still shows the event.
var read = protojson.UnmarshalOptions{DiscardUnknown: true}

// Decode reads one stored event: its kind, its payload version and its payload.
func Decode(kind string, version int16, payload []byte) (*taskv1alpha1.TimelineEvent, error) {
	fd, ok := byKind[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	switch version {
	case VersionLegacy:
		return decodeLegacy(fd, payload)
	case VersionTyped:
		ev := &taskv1alpha1.TimelineEvent{}
		if err := read.Unmarshal(payload, ev); err != nil {
			return nil, fmt.Errorf("read a %q event: %w", kind, err)
		}
		if got, _ := Kind(ev); got != kind {
			return nil, fmt.Errorf("a %q row holds a %q event", kind, got)
		}
		return ev, nil
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnknownVersion, version)
	}
}

// Encode returns the kind and the version 1 payload of an event.
func Encode(ev *taskv1alpha1.TimelineEvent) (kind string, payload []byte, err error) {
	kind, ok := Kind(ev)
	if !ok {
		return "", nil, ErrUnknownKind
	}
	payload, err = protojson.Marshal(ev)
	if err != nil {
		return "", nil, fmt.Errorf("write a %q event: %w", kind, err)
	}
	return kind, payload, nil
}

// decodeLegacy translates a version 0 payload into the member fd.
func decodeLegacy(fd protoreflect.FieldDescriptor, payload []byte) (*taskv1alpha1.TimelineEvent, error) {
	obj := map[string]any{}
	if len(bytes.TrimSpace(payload)) > 0 && !bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber() // keep integers exact
		if err := dec.Decode(&obj); err != nil {
			return nil, fmt.Errorf("read a version 0 %q payload: %w", catalog[fd.Name()].kind, err)
		}
	}
	catalog[fd.Name()].legacy(obj)
	shaped, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	ev := &taskv1alpha1.TimelineEvent{}
	m := ev.ProtoReflect()
	member := m.NewField(fd).Message()
	if err := read.Unmarshal(shaped, member.Interface()); err != nil {
		return nil, fmt.Errorf("read a version 0 %q payload: %w", catalog[fd.Name()].kind, err)
	}
	m.Set(fd, protoreflect.ValueOfMessage(member))
	return ev, nil
}
