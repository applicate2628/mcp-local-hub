package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

func newAttachClientCmd() *cobra.Command {
	a := api.NewAPI()
	return newAttachClientCmdWithDeps(attachClientCmdDeps{
		buildAttach: a.BuildAttachClientPlan, buildRollback: a.BuildAttachClientRollbackPlan,
		applyAttach: a.ApplyAttachClientPlan, applyRollback: a.RollbackAttachClient,
	})
}

func newAttachClientCmdWithPlanBuilder(buildPlan func(context.Context, api.AttachClientRequest) (*api.AttachClientPlan, error)) *cobra.Command {
	return newAttachClientCmdWithDeps(attachClientCmdDeps{buildAttach: buildPlan})
}

type attachClientCmdDeps struct {
	buildAttach   func(context.Context, api.AttachClientRequest) (*api.AttachClientPlan, error)
	buildRollback func(context.Context, api.AttachClientRequest, string) (*api.AttachClientPlan, error)
	applyAttach   func(context.Context, *api.AttachClientPlan) (*api.AttachClientApplyResult, error)
	applyRollback func(context.Context, api.AttachClientRequest, string) (*api.AttachClientApplyResult, error)
}

func attachClientPlanFailure(err error, stage string) error {
	var planErr *api.AttachPlanError
	if errors.As(err, &planErr) {
		return fmt.Errorf("ATTACH_%s stage=%s", strings.ToUpper(strings.ReplaceAll(planErr.Code, "-", "_")), stage)
	}
	return fmt.Errorf("ATTACH_PLAN_FAILED stage=%s", stage)
}

func attachClientMutationFailure(err error, stage string) error {
	var applyErr *api.AttachClientApplyError
	if errors.As(err, &applyErr) {
		return fmt.Errorf("%s stage=%s", applyErr.Code, applyErr.Stage)
	}
	return fmt.Errorf("ATTACH_%s_FAILED stage=%s", strings.ToUpper(stage), stage)
}

func newAttachClientCmdWithDeps(deps attachClientCmdDeps) *cobra.Command {
	var daemon, client, manifestSource string
	var apply bool
	var rollbackReceipt string
	cmd := &cobra.Command{
		Use:           "attach-client <server> --daemon <daemon> --client <client> [--manifest-source <file>] [--rollback-receipt <receipt-id>] [--apply]",
		Short:         "Preview or apply one client attachment or exact-receipt rollback",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return errors.New("ATTACH_USAGE stage=usage")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if (cmd.Flags().Changed("apply") && !apply) || !cmd.Flags().Changed("daemon") || !cmd.Flags().Changed("client") ||
				strings.TrimSpace(daemon) == "" || strings.TrimSpace(client) == "" ||
				(cmd.Flags().Changed("rollback-receipt") && (strings.TrimSpace(rollbackReceipt) == "" || strings.EqualFold(rollbackReceipt, "latest") || strings.EqualFold(rollbackReceipt, "all") || cmd.Flags().Changed("manifest-source"))) ||
				(cmd.Flags().Changed("manifest-source") && strings.TrimSpace(manifestSource) == "") {
				return errors.New("ATTACH_USAGE stage=usage")
			}
			req := api.AttachClientRequest{Server: args[0], Daemon: daemon, Client: client, ManifestSource: manifestSource}
			rollback := cmd.Flags().Changed("rollback-receipt")
			var plan *api.AttachClientPlan
			var err error
			if rollback {
				plan, err = deps.buildRollback(cmd.Context(), req, rollbackReceipt)
			} else {
				plan, err = deps.buildAttach(cmd.Context(), req)
			}
			if err != nil {
				if rollback {
					return attachClientPlanFailure(err, "rollback-plan")
				}
				return attachClientPlanFailure(err, "plan")
			}
			if plan == nil || plan.Server != args[0] || plan.Daemon != daemon || plan.Client != client ||
				plan.EntryKey == "" ||
				plan.TargetBytesSHA256 == "" || plan.EntryTokenSHA256 == "" {
				if rollback {
					return errors.New("ATTACH_PLAN_INVALID stage=rollback-plan")
				}
				return errors.New("ATTACH_PLAN_INVALID stage=plan")
			}
			operation, state := "attach", "would-attach"
			if rollback {
				operation = "rollback"
				state = plan.State
				if plan.ReceiptID != rollbackReceipt || (state != "would-rollback" && state != "already-rolled-back") ||
					plan.AfterTransport != "direct" || (plan.BeforeTransport != "http" && plan.BeforeTransport != "direct") {
					return errors.New("ATTACH_PLAN_INVALID stage=rollback-plan")
				}
			} else if plan.State == "already-committed" || plan.State == "would-restore-and-attach" {
				state = plan.State
			} else if plan.State != "" && plan.State != "would-attach" {
				return errors.New("ATTACH_PLAN_INVALID stage=plan")
			}
			if !rollback && (((state == "would-attach" || state == "would-restore-and-attach") && (plan.BeforeTransport != "direct" || plan.AfterTransport != "http")) ||
				(state == "already-committed" && (plan.BeforeTransport != "http" || plan.AfterTransport != "http"))) {
				return errors.New("ATTACH_PLAN_INVALID stage=plan")
			}
			if state == "would-restore-and-attach" && (len(plan.ManifestSHA256) != 64 || (plan.ManifestProvenance != "explicit-file" && plan.ManifestProvenance != "adopted-codex-snapshot")) {
				return errors.New("ATTACH_PLAN_INVALID stage=plan")
			}
			if !apply {
				if rollback {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "operation=rollback state=%s server=%q daemon=%q client=%q entry=%q receipt_id=%q transport=%s->%s target_bytes_sha256=%s entry_token_sha256=%s\n",
						state, plan.Server, plan.Daemon, plan.Client, plan.EntryKey, rollbackReceipt, plan.BeforeTransport, plan.AfterTransport, plan.TargetBytesSHA256, plan.EntryTokenSHA256)
				} else {
					if state == "would-restore-and-attach" {
						_, err = fmt.Fprintf(cmd.OutOrStdout(), "operation=attach state=%s server=%q daemon=%q client=%q entry=%q transport=%s->%s target_bytes_sha256=%s entry_token_sha256=%s manifest_restore=exact manifest_sha256=%s manifest_provenance=%s legacy_lease_namespace_may_migrate=true\n",
							state, plan.Server, plan.Daemon, plan.Client, plan.EntryKey, plan.BeforeTransport, plan.AfterTransport, plan.TargetBytesSHA256, plan.EntryTokenSHA256, plan.ManifestSHA256, plan.ManifestProvenance)
					} else {
						_, err = fmt.Fprintf(cmd.OutOrStdout(), "operation=attach state=%s server=%q daemon=%q client=%q entry=%q transport=%s->%s target_bytes_sha256=%s entry_token_sha256=%s\n",
							state, plan.Server, plan.Daemon, plan.Client, plan.EntryKey, plan.BeforeTransport, plan.AfterTransport, plan.TargetBytesSHA256, plan.EntryTokenSHA256)
					}
				}
			} else {
				var result *api.AttachClientApplyResult
				if rollback {
					result, err = deps.applyRollback(cmd.Context(), req, rollbackReceipt)
				} else {
					result, err = deps.applyAttach(cmd.Context(), plan)
				}
				if err != nil {
					if rollback {
						return attachClientMutationFailure(err, "rollback")
					}
					return attachClientMutationFailure(err, "apply")
				}
				if result == nil || result.Server != req.Server || result.Daemon != req.Daemon || result.Client != req.Client || result.EntryKey != plan.EntryKey || result.ReceiptID == "" || result.BeforeSHA256 == "" || result.AfterSHA256 == "" {
					return errors.New("ATTACH_RESULT_INVALID stage=apply")
				}
				before, after := "direct", "http"
				if rollback {
					if result.ReceiptID != rollbackReceipt || result.State != "rolled-back" {
						return errors.New("ATTACH_RESULT_INVALID stage=rollback")
					}
					before, after = "http", "direct"
					state = "rolled-back"
					if result.IdempotentNoWrite {
						before, state = "direct", "already-rolled-back"
					}
				} else {
					if (result.State != "committed" && result.State != "already-committed") ||
						(!result.IdempotentNoWrite && result.State != "committed") {
						return errors.New("ATTACH_RESULT_INVALID stage=apply")
					}
					state = result.State
					if result.IdempotentNoWrite {
						before, state = "http", "already-committed"
					}
				}
				if result.ManifestRestored {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "operation=%s state=%s server=%q daemon=%q client=%q entry=%q receipt_id=%q transport=%s->%s before_sha256=%s after_sha256=%s entry_token_sha256=%s manifest_restored=true manifest_sha256=%s lease_namespace_before=%s lease_namespace_after=%s\n",
						operation, state, result.Server, result.Daemon, result.Client, result.EntryKey, result.ReceiptID, before, after, result.BeforeSHA256, result.AfterSHA256, plan.EntryTokenSHA256, result.ManifestSHA256, result.LeaseNamespaceBefore, result.LeaseNamespaceAfter)
				} else {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "operation=%s state=%s server=%q daemon=%q client=%q entry=%q receipt_id=%q transport=%s->%s before_sha256=%s after_sha256=%s entry_token_sha256=%s\n",
						operation, state, result.Server, result.Daemon, result.Client, result.EntryKey, result.ReceiptID, before, after, result.BeforeSHA256, result.AfterSHA256, plan.EntryTokenSHA256)
				}
			}
			if err != nil {
				return errors.New("ATTACH_OUTPUT_FAILED stage=output")
			}
			return nil
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error { return errors.New("ATTACH_USAGE stage=usage") })
	cmd.Flags().StringVar(&daemon, "daemon", "", "existing managed daemon to select")
	cmd.Flags().StringVar(&client, "client", "", "client to attach (mimocode only in this slice)")
	cmd.Flags().StringVar(&manifestSource, "manifest-source", "", "optional exact manifest file if no retained product source is eligible")
	cmd.Flags().StringVar(&rollbackReceipt, "rollback-receipt", "", "exact current attachment receipt ID to roll back")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the selected attach or rollback after API revalidation")
	return cmd
}
