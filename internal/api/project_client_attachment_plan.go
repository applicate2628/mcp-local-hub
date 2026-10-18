package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"mcp-local-hub/internal/clients"
)

// InstallProjectClientCreate is an explicit prospective project attachment.
// A nil pointer leaves every legacy installation route unchanged.
type InstallProjectClientCreate struct {
	ProjectRoot        string
	LogicalEntryName   string
	PolicySource       string
	ApplyCreate        bool
	ExpectedPlanSHA256 string
}

func validateInstallProjectClientCreate(opts InstallOpts) error {
	c := opts.ProjectClientCreate
	if c == nil {
		return nil
	}
	if opts.Server == "" || opts.DaemonFilter == "" || len(opts.ClientsInclude) != 1 || opts.ClientsInclude[0] != "codex-cli" || opts.IncludeAllClients || opts.SkipClientConfigWrites || opts.CodexProjectRoot != "" || opts.CodexWorkingDir != "" || !filepath.IsAbs(c.ProjectRoot) || !managedRegistrationSafeIdentifier(c.LogicalEntryName) || (c.PolicySource != "inherited-global" && c.PolicySource != "native-defaults") {
		return clients.ErrProjectClientInvalid
	}
	if opts.DryRun {
		if c.ApplyCreate || c.ExpectedPlanSHA256 != "" {
			return clients.ErrProjectClientInvalid
		}
	} else if !c.ApplyCreate || !managedRegistrationBackupDigestValid(c.ExpectedPlanSHA256) {
		return clients.ErrProjectClientInvalid
	}
	return nil
}

// ValidateInstallProjectClientCreate is the same omission-safe admission used
// by the install entry point; CLI flag-presence validation adds no API policy.
func ValidateInstallProjectClientCreate(opts InstallOpts) error {
	return validateInstallProjectClientCreate(opts)
}

// Declaration order is the plan's wire digest contract. Paths, keys, full
// policy values and beforeimages are deliberately absent from this public data.
type projectClientPlanBinding struct {
	Schema                string `json:"schema"`
	Operation             string `json:"operation"`
	Client                string `json:"client"`
	ProjectIdentitySHA256 string `json:"project_identity_sha256"`
	ConfigPathSHA256      string `json:"config_path_sha256"`
	ConfigBeforeSHA256    string `json:"config_before_sha256"`
	LogicalKeySHA256      string `json:"logical_key_sha256"`
	ManagedKeySHA256      string `json:"managed_key_sha256"`
	PolicySource          string `json:"policy_source"`
	PolicySHA256          string `json:"policy_sha256"`
	ManifestSHA256        string `json:"manifest_sha256"`
	TargetGroupSHA256     string `json:"target_group_sha256"`
	DesiredEntrySHA256    string `json:"desired_entry_sha256"`
	ReleaseIdentitySHA256 string `json:"release_identity_sha256"`
}
type ProjectClientCreatePlanV1 struct {
	projectClientPlanBinding
	PlanSHA256 string `json:"plan_sha256"`
}

type projectClientAttachmentPlan struct {
	public      ProjectClientCreatePlanV1
	preparation *clients.ProjectHTTPAttachmentPreparation
	project     clients.Client
	identity    clients.CodexProjectIdentity
	global      []byte
}

func projectClientHash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func projectClientValueHash(v any) (string, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	return projectClientHash(b), nil
}

func (p ProjectClientCreatePlanV1) validate() error {
	validPair := p.Schema == "project-client-create-plan-v1" && p.Operation == "create-absent" || p.Schema == "project-client-attachment-plan-v2" && p.Operation == "replace-enable-override"
	if !validPair || p.Client != "codex-cli" || (p.PolicySource != "inherited-global" && p.PolicySource != "native-defaults") {
		return clients.ErrProjectClientInvalid
	}
	for _, h := range []string{p.ProjectIdentitySHA256, p.ConfigPathSHA256, p.ConfigBeforeSHA256, p.LogicalKeySHA256, p.ManagedKeySHA256, p.PolicySHA256, p.ManifestSHA256, p.TargetGroupSHA256, p.DesiredEntrySHA256, p.ReleaseIdentitySHA256, p.PlanSHA256} {
		if !managedRegistrationBackupDigestValid(h) {
			return clients.ErrProjectClientInvalid
		}
	}
	h, err := projectClientValueHash(p.projectClientPlanBinding)
	if err != nil || h != p.PlanSHA256 {
		return clients.ErrProjectClientPlanStale
	}
	return nil
}

func DecodeProjectClientCreatePlan(data []byte) (ProjectClientCreatePlanV1, error) {
	var p ProjectClientCreatePlanV1
	if err := decodeProjectAttachmentJSON(data, &p); err != nil {
		return p, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || !projectClientExactJSONFields(fields, "schema", "operation", "client", "project_identity_sha256", "config_path_sha256", "config_before_sha256", "logical_key_sha256", "managed_key_sha256", "policy_source", "policy_sha256", "manifest_sha256", "target_group_sha256", "desired_entry_sha256", "release_identity_sha256", "plan_sha256") {
		return p, clients.ErrProjectClientInvalid
	}
	return p, p.validate()
}

func projectClientExactJSONFields(fields map[string]json.RawMessage, names ...string) bool {
	if len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

// Use the existing project factory and raw policy owner. The injected global
// snapshot has already left its own read lock; there is no inverted lock order.
func prepareProjectClientAttachmentPlan(opts InstallOpts, manifestSHA string, plan *Plan, project clients.Client, identity clients.CodexProjectIdentity, global []byte, release managedRegistrationReleaseIdentity) (*projectClientAttachmentPlan, error) {
	if err := validateInstallProjectClientCreate(opts); err != nil {
		return nil, err
	}
	if opts.ProjectClientCreate == nil || plan == nil || project == nil || len(plan.ClientUpdates) != 1 || len(plan.SupervisorIntent) != 1 || !managedRegistrationBackupDigestValid(manifestSHA) || release.Version == "" || release.Commit == "" {
		return nil, clients.ErrProjectClientInvalid
	}
	c := opts.ProjectClientCreate
	u := plan.ClientUpdates[0]
	if u.Client != "codex-cli" || u.DaemonName != opts.DaemonFilter || u.URL == "" || u.Action != ClientUpdateAddReplace {
		return nil, clients.ErrProjectClientInvalid
	}
	owner, ok := project.(clients.ProjectHTTPAttachmentClient)
	if !ok {
		return nil, clients.ErrProjectClientInvalid
	}
	desired := installClientEntryV1(c.LogicalEntryName, u, plan.installRelayExePath)
	desired.Name = c.LogicalEntryName + "-mcphub"
	p, err := owner.PrepareProjectHTTPAttachment(c.LogicalEntryName, desired, c.PolicySource, global)
	if err != nil {
		return nil, err
	}
	schema, operation := p.SchemaOperation()
	desired = p.DesiredEntry()
	policy := make(map[string]any)
	for k, v := range desired.Raw {
		switch k {
		case "command", "args", "env", "cwd", "env_vars", "url", "http_headers":
		default:
			policy[k] = v
		}
	}
	policySHA, err := projectClientValueHash(policy)
	if err != nil {
		return nil, err
	}
	desiredSHA, err := projectClientValueHash(desired.Raw)
	if err != nil {
		return nil, err
	}
	targetSHA, err := projectClientValueHash(plan.SupervisorIntent)
	if err != nil {
		return nil, err
	}
	releaseSHA, err := projectClientValueHash(release)
	if err != nil {
		return nil, err
	}
	public := ProjectClientCreatePlanV1{projectClientPlanBinding: projectClientPlanBinding{Schema: schema, Operation: operation, Client: "codex-cli", ProjectIdentitySHA256: identity.ProjectIdentitySHA256(), ConfigPathSHA256: projectClientHash([]byte(project.ConfigPath())), ConfigBeforeSHA256: p.BeforeSHA256(), LogicalKeySHA256: projectClientHash([]byte(c.LogicalEntryName)), ManagedKeySHA256: projectClientHash([]byte(desired.Name)), PolicySource: c.PolicySource, PolicySHA256: policySHA, ManifestSHA256: manifestSHA, TargetGroupSHA256: targetSHA, DesiredEntrySHA256: desiredSHA, ReleaseIdentitySHA256: releaseSHA}}
	public.PlanSHA256, err = projectClientValueHash(public.projectClientPlanBinding)
	if err != nil {
		return nil, err
	}
	if !opts.DryRun && public.PlanSHA256 != c.ExpectedPlanSHA256 {
		return nil, clients.ErrProjectClientPlanStale
	}
	return &projectClientAttachmentPlan{public: public, preparation: p, project: project, identity: identity, global: append([]byte(nil), global...)}, nil
}

// Strict token walk rejects duplicate fields, null authority and trailing
// values before the typed decoder rejects unknown keys. It is confined to the
// plan/private-capsule boundary, not a new configuration parser.
func decodeProjectAttachmentJSON(data []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(bool) error
	value = func(allowNull bool) error {
		t, e := d.Token()
		if e != nil {
			return e
		}
		if t == nil {
			if allowNull {
				return nil
			}
			return clients.ErrProjectClientInvalid
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return e
					}
					key, ok := k.(string)
					if !ok || seen[key] {
						return clients.ErrProjectClientInvalid
					}
					seen[key] = true
					if e = value(key == "MutationRows" || key == "ClientConfigSettlements"); e != nil {
						return e
					}
				}
				end, e := d.Token()
				if e != nil || end != json.Delim('}') {
					return clients.ErrProjectClientInvalid
				}
			case '[':
				for d.More() {
					if e := value(false); e != nil {
						return e
					}
				}
				end, e := d.Token()
				if e != nil || end != json.Delim(']') {
					return clients.ErrProjectClientInvalid
				}
			default:
				return clients.ErrProjectClientInvalid
			}
		}
		return nil
	}
	if err := value(false); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return clients.ErrProjectClientInvalid
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return err
	}
	return nil
}
