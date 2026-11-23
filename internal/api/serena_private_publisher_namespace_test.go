package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestPrivateSerenaPublisherNamespaceRejectsOrdinaryAndPartialOwners(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	end, _ := ctx.Deadline()
	assembly := &privatePublisherRefusingAssembly{}
	ordinary := NewAPIWithSerenaPreparation(assembly)
	partial := NewAPIWithSerenaPreparation(assembly)
	partial.serenaPrivatePublisher = &privateSerenaPublisherOwner{
		api: partial, publication: partial.serenaPreparationPublication, original: ctx, deadline: end,
	}
	canceledContext, cancelOriginal := context.WithDeadline(context.Background(), end)
	cancelOriginal()
	canceled := NewAPIWithSerenaPreparation(assembly)
	canceled.serenaPrivatePublisher = &privateSerenaPublisherOwner{
		api: canceled, publication: canceled.serenaPreparationPublication, original: canceledContext, deadline: end,
	}
	for name, a := range map[string]*API{"nil": nil, "ordinary": ordinary, "partial": partial, "original_canceled": canceled} {
		t.Run(name, func(t *testing.T) {
			observation, err := a.ObservePrivateSerenaPublisherNamespace(ctx)
			if observation != nil || err == nil {
				t.Fatalf("unqualified owner reached namespace acquisition: observation=%v error=%v", observation != nil, err)
			}
			if a == canceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("original cancellation lost: %v", err)
			}
			if a != nil && a.serenaPrivatePublisher != nil && a.serenaPrivatePublisher.namespaceObservation != nil {
				t.Fatal("namespace acquisition attempted before private admission")
			}
		})
	}
	assembly.assertUnused(t)
}

func TestPrivateSerenaPublisherNamespaceSnapshotDoesNotIssueAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a := NewAPI()
	raw, err := json.Marshal(PrivateSerenaPublisherNamespaceSnapshot{
		Schema: "private-serena-publisher-namespace-v1",
		Bound:  true, NamespaceClosed: true, Complete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded PrivateSerenaPublisherNamespaceSnapshot
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for name, observation := range map[string]*PrivateSerenaPublisherNamespaceObservation{
		"nil":                nil,
		"empty":              {},
		"reconstructed_data": {view: decoded, finished: true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := observation.Validate(ctx, a); err == nil {
				t.Fatal("serialized completion flags supplied original namespace authority")
			}
		})
	}
}
