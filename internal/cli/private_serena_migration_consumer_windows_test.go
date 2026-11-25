//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemon"
)

const privateSerenaNativeConsumerRoot = "TestMigrateSerena_CallsInstallParsedManifest_NotDiskWrite"

// Keep the original owners reachable through the case. A DATA receipt below
// reports their receiving; it cannot reconstruct or release any of them.
type privateSerenaMigrationNativeConsumer struct {
	launch          *api.PreparedSerenaLaunchObservation
	owner           *api.PreparedSerenaRuntimeOwner
	host            *daemon.HTTPHost
	adapter         *preparedConsumerLifecycleAdapter
	start           *daemon.HTTPHostStartReceipt
	terminal        *daemon.HTTPHostTerminalReceipt
	relay           *daemon.HTTPToStdioRelay
	protocol        *daemon.MCPHTTPQualificationObservation
	protocolClose   *daemon.MCPHTTPQualificationCloseObservation
	protocolBinding daemon.MCPHTTPQualificationBinding
}

// This is the one post-Ready consumer of the same original parsed preparation.
// It uses the production admission, native host and MCP qualification owners.
func (c *privateSerenaMigrationCase) consumeNative(installer *api.API, operation migrateSerenaInstallOperation, task string) (receipt *privateSerenaNativeConsumerReceipt, retErr error) {
	if c == nil || installer == nil || operation.observeConsumerLaunch == nil || task == "" || c.root.Name != privateSerenaNativeConsumerRoot {
		return nil, errors.New("private migration native consumer source is missing")
	}
	original := &privateSerenaMigrationNativeConsumer{}
	c.nativeConsumers = append(c.nativeConsumers, original)
	var err error
	original.launch, err = operation.observeConsumerLaunch(c.ctx, task)
	if err != nil {
		return nil, err
	}
	descriptor, err := original.launch.Descriptor(c.ctx)
	if err != nil {
		return nil, err
	}
	if descriptor.TaskName != task || descriptor.Workspace == "" || descriptor.RuntimeSpec == nil || descriptor.RuntimeSpec.PreparedRuntime == nil {
		return nil, errors.New("private migration native consumer requires its installed workspace descriptor")
	}
	ready := *descriptor.RuntimeSpec.PreparedRuntime
	start, cancelStart, err := api.PreparedSerenaStartContext(c.ctx, time.Duration(api.EffectiveStartupBindDeadlineSeconds(descriptor))*time.Second)
	if err != nil {
		return nil, err
	}
	defer cancelStart()
	cleanupEnd, err := daemon.PreparedHTTPHostStartupCleanupDeadline(start)
	if err != nil || cleanupEnd.After(c.config.WorkEnd) {
		return nil, errors.Join(err, errors.New("private migration native consumer has no original startup and cleanup interval"))
	}
	original.owner, err = installer.PreparedSerenaConsumerOwnerObserved(start, original.launch)
	if err != nil {
		return nil, err
	}
	cfg, err := preparedSerenaHTTPHostConfig(start, original.launch, descriptor, filepath.Join(c.config.Directory, "native-consumer.log"))
	if err != nil {
		return nil, err
	}
	original.host, original.adapter, err = newPreparedConsumerHTTPHost(start, original.owner, descriptor, cfg, original.launch)
	if err != nil {
		return nil, err
	}
	var confirmed api.PreparedConsumerBindingV1
	defer func() {
		// One common existing host cleanup budget covers protocol close and
		// native/IO settlement. It is also clipped to the prearmed absolute end.
		parent, cancelParent := context.WithDeadline(context.WithoutCancel(c.ctx), cleanupEnd)
		defer cancelParent()
		cleanup, cancelCleanup, cleanupErr := original.host.PreparedStopContext(parent)
		if cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
			return
		}
		defer cancelCleanup()
		if original.relay != nil {
			var closeErr error
			original.protocolClose, closeErr = original.relay.CloseSessionObserved(cleanup)
			retErr = errors.Join(retErr, closeErr)
			if original.protocol != nil {
				retErr = errors.Join(retErr, original.protocol.ReceiveClose(cleanup, original.protocolClose))
			}
		}
		if retErr == nil {
			retErr = original.host.ReceiveQualificationProtocol(cleanup, original.protocol, original.protocolBinding)
		}
		terminal, stopErr := original.host.StopObserved(cleanup)
		original.terminal = &terminal
		retErr = errors.Join(retErr, stopErr, terminal.ValidateOriginalHost(cleanup, original.host))
		if retErr != nil {
			return
		}
		if terminal.NoNativeAcquisition || !terminal.NativeSettlement.Complete() || terminal.NativeSettlement.Snapshot().NoChildKnown {
			retErr = errors.New("private migration native consumer did not receive an actual child closure")
			return
		}
		finalBinding, bindingErr := original.adapter.ticket.Binding()
		retErr = errors.Join(bindingErr, original.adapter.ValidateOriginalBinding(cleanup, finalBinding), original.adapter.ValidateOriginalIO(cleanup, finalBinding), original.protocol.Validate(cleanup, original.protocolBinding))
		if retErr != nil {
			return
		}
		order, orderErr := terminal.QualificationEventOrder(original.host)
		if orderErr != nil {
			retErr = orderErr
			return
		}
		received := &privateSerenaNativeConsumerReceipt{
			Schema: privateSerenaNativeConsumerSchema, Ready: ready, Confirmed: confirmed, Terminal: finalBinding,
			Protocol: privateSerenaNativeConsumerProtocol{
				QualificationID: original.protocolBinding.QualificationID,
				HostStartSHA256: original.protocolBinding.HostStartSHA256,
				EndpointSHA256:  original.protocolBinding.EndpointSHA256,
				ReceiptSHA256:   original.protocol.ReceiptSHA256(),
			},
			Native: terminal.NativeSettlement.Snapshot(), IO: terminal.IOSettlement,
			IOReceiptSHA256: terminal.OriginalIOObservation().ReceiptSHA256(), Order: order, Complete: true,
		}
		if retErr = validatePrivateSerenaNativeConsumer(received, c.loaded.Input); retErr == nil {
			receipt = received
		}
	}()
	started, err := original.host.StartObserved(start)
	original.start = &started
	if err != nil {
		return nil, err
	}
	if err = started.ValidateOriginalHost(start, original.host); err != nil || !started.OriginalCreated() || !started.Ready || !started.OriginalLive || started.Error != nil {
		return nil, errors.Join(err, started.Error, errors.New("private migration native consumer start is unconfirmed"))
	}
	confirmed, err = original.adapter.ticket.Binding()
	if err != nil || confirmed.Disposition != "current" || confirmed.StartDisposition != "confirmed" || confirmed.NativeBinding == nil || confirmed.ClosureReceiptSHA256 != nil {
		return nil, errors.Join(err, errors.New("private migration native consumer current binding is unconfirmed"))
	}
	if err = errors.Join(confirmed.Validate(), started.OriginalStart.Validate(start, confirmed.NativeBinding.PID, confirmed.NativeBinding.NativeBirthFILETIME)); err != nil {
		return nil, err
	}
	endpoint, err := started.ExpectedEndpoint(start)
	if err != nil {
		return nil, err
	}
	endpointSource, err := started.OriginalEndpoint(start)
	if err != nil {
		return nil, err
	}
	qualificationID, err := api.NewSerenaConsumerOperationID(start)
	if err != nil {
		return nil, err
	}
	identity, err := json.Marshal(started.OriginalStart.Identity())
	if err != nil {
		return nil, err
	}
	original.protocolBinding = daemon.MCPHTTPQualificationBinding{
		OperationID: confirmed.OperationID, QualificationID: qualificationID,
		RequestIdentitySHA256: confirmed.PackageRequestIdentitySHA256, GenerationID: confirmed.GenerationID,
		SourceSHA256: confirmed.SourceSHA256, HostStartSHA256: preparedQualificationSHA(identity),
		EndpointSHA256: preparedQualificationSHA([]byte(endpoint)), EndpointSource: endpointSource,
	}
	original.relay = &daemon.HTTPToStdioRelay{URL: endpoint}
	original.protocol, err = original.relay.QualifyMCPObserved(start, original.protocolBinding)
	return nil, err
}
