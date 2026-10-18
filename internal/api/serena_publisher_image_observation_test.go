package api

import (
	"context"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

func TestGenuineImageRequiresOriginalNativeFacetBeforeBinding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel()
	for _, native := range []*process.PublisherNativeObservation{nil, {}} {
		if image, err := NewCurrentPublisherImageObservation(ctx, native); err == nil || image != nil { t.Fatal("missing or DTO native facet accepted") }
	}
	if (&CurrentPublisherImageObservation{}).Validate(ctx, &process.PublisherNativeObservation{}) == nil { t.Fatal("foreign image original accepted") }
	if (&PublisherImageObservation{}).Validate(ctx, &process.PublisherIncarnation{}, "") == nil { t.Fatal("bound image obligation weakened") }
}
