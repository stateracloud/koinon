// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"context"
	"net/http"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/stateracloud/koinon/internal/adminapi"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/tokens"
	"github.com/stateracloud/koinon/pricing"
)

// A held install answers the admin REST API's outward calls with 409 and the hold message: the
// service's restorehold.ErrHeld is mapped here, as the Connect surface maps it to FailedPrecondition
// (issue #462, ADR 0107).
func TestEchoCheckIsRefusedWhileHeld(t *testing.T) {
	check := modelcheck.New(nil, nil, prices.Resolver{}).WithRestoreHold(restorehold.NewGuard(true))
	s := adminapi.New(nil, &catalog.Service{}, &tokens.Service{}, nil, pricing.Embedded()).WithModelCheck(check)
	res, err := s.CheckProviderEcho(context.Background(), adminapi.CheckProviderEchoRequestObject{
		ProviderId: openapi_types.UUID{1}, Body: &adminapi.CheckProviderEchoJSONRequestBody{Model: "m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := res.(adminapi.CheckProviderEchodefaultJSONResponse)
	if !ok {
		t.Fatalf("response = %T", res)
	}
	if got.StatusCode != http.StatusConflict || got.Body.Error.Message != restorehold.Message {
		t.Fatalf("status %d, message %q", got.StatusCode, got.Body.Error.Message)
	}
}

// A held install answers an enable of an outward target with 409 and the hold message, before it reads or
// writes anything; a disable is not refused by the hold (ADR 0110 decision 5).
func TestEnableOutwardTargetIsRefusedWhileHeld(t *testing.T) {
	s := adminapi.New(nil, &catalog.Service{}, &tokens.Service{}, nil, pricing.Embedded()).
		WithOutward(outward.New(nil).WithRestoreHold(restorehold.NewGuard(true)))
	res, err := s.SetOutwardTargetDisabled(context.Background(), adminapi.SetOutwardTargetDisabledRequestObject{
		Kind: adminapi.OutwardTargetKindMcpServer, TargetId: openapi_types.UUID{1}, Body: &adminapi.SetOutwardTargetDisabledJSONRequestBody{Disabled: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := res.(adminapi.SetOutwardTargetDisableddefaultJSONResponse)
	if !ok {
		t.Fatalf("response = %T", res)
	}
	if got.StatusCode != http.StatusConflict || got.Body.Error.Message != restorehold.Message {
		t.Fatalf("status %d, message %q", got.StatusCode, got.Body.Error.Message)
	}
}
