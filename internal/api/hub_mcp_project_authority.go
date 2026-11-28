package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This key is owned by the backend routing state and stays outside SettingsRegistry.
const hubProjectAuthoritySettingsKey = "mcp_front.project_group_authority"

var errHubProjectAuthority = errors.New("hub project authorization unavailable")

type hubProjectAuthorityRecord struct {
	Version int                             `json:"version"`
	Groups  map[string]hubProjectEnrollment `json:"groups"`
	epoch   MCPFrontRoutingTargetSnapshot
}

type hubProjectEnrollment struct {
	Group             string `json:"group"`
	Project           string `json:"project"`
	Revision          string `json:"revision"`
	PriorRevision     string `json:"prior_revision,omitempty"`
	Generation        int    `json:"generation"`
	Port              int    `json:"port"`
	AttachmentPlan    string `json:"attachment_plan"`
	AttachmentReceipt string `json:"attachment_receipt,omitempty"`
	Credential        string `json:"credential_verifier,omitempty"`
	ProjectRoot       string `json:"project_root,omitempty"`
	CapsuleLeaf       string `json:"capsule_leaf,omitempty"`
	GroupSHA256       string `json:"group_sha256,omitempty"`
	Revoked           bool   `json:"revoked,omitempty"`
}

func hubProjectAuthorityFromSettings(raw map[string]string) (hubProjectAuthorityRecord, error) {
	encoded, present := raw[hubProjectAuthoritySettingsKey]
	if !present {
		return hubProjectAuthorityRecord{Version: 1, Groups: map[string]hubProjectEnrollment{}}, nil
	}
	var record hubProjectAuthorityRecord
	if decodeProjectAttachmentJSON([]byte(encoded), &record) != nil || record.Version != 1 || record.Groups == nil {
		return hubProjectAuthorityRecord{}, errHubProjectAuthority
	}
	for key, enrollment := range record.Groups {
		if key != hubProjectGroupKey(enrollment.Group) || !enrollment.valid() {
			return hubProjectAuthorityRecord{}, errHubProjectAuthority
		}
	}
	epoch, err := mcpFrontRoutingEpochFromRaw(raw, true)
	if err != nil {
		return hubProjectAuthorityRecord{}, errHubProjectAuthority
	}
	record.epoch = epoch
	return record, nil
}

func readHubProjectAuthority() (hubProjectAuthorityRecord, error) {
	path := SettingsPath()
	if path == "" {
		return hubProjectAuthorityRecord{}, errHubProjectAuthority
	}
	raw, err := readHubProjectSettingsMap(path)
	if err != nil {
		return hubProjectAuthorityRecord{}, errHubProjectAuthority
	}
	return hubProjectAuthorityFromSettings(raw)
}

func readHubProjectSettingsMap(path string) (map[string]string, error) {
	data, err := readStateFileInodeAnchored(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, errHubProjectAuthority
	}
	raw, err := decodeSingleSettingsDocument(data)
	if err != nil {
		return nil, errHubProjectAuthority
	}
	migrateLegacyKeys(raw)
	return raw, nil
}

func hubProjectGroupKey(group string) string { return strings.ToLower(group) }

func (record hubProjectAuthorityRecord) guards(scope string) bool {
	group, found := strings.CutPrefix(scope, GroupScopeKeyPrefix)
	if !found {
		return false
	}
	_, qualified := record.Groups[hubProjectGroupKey(group)]
	return qualified
}

func (e hubProjectEnrollment) valid() bool {
	if e.ProjectRoot != "" || e.CapsuleLeaf != "" || e.GroupSHA256 != "" {
		if !filepath.IsAbs(e.ProjectRoot) || filepath.Base(e.CapsuleLeaf) != e.CapsuleLeaf || !strings.HasSuffix(e.CapsuleLeaf, ".project-client-private-v3.json") || !isLowerHex64(e.GroupSHA256) {
			return false
		}
	}
	return validateGroupName(e.Group) == nil && isLowerHex64(e.Project) && isLowerHex64(e.Revision) &&
		(e.PriorRevision == "" || isLowerHex64(e.PriorRevision) && e.PriorRevision != e.Revision) &&
		e.Generation > 0 && e.Port > 0 && e.Port <= 65535 && isLowerHex64(e.AttachmentPlan) &&
		(e.Credential == "" || isLowerHex64(e.Credential)) &&
		(e.AttachmentReceipt == "" || isLowerHex64(e.AttachmentReceipt) && e.Credential != "")
}

func (e hubProjectEnrollment) matchesEpoch(epoch MCPFrontRoutingTargetSnapshot) bool {
	return epoch.State == MCPFrontRoutingTargetFront && e.Generation == epoch.Generation && e.Port == epoch.Port
}

func (e hubProjectEnrollment) verifier(token string) string {
	sum := sha256.Sum256([]byte("mcphub-project-group-v1\x00" + GroupScopeKey(e.Group) + "\x00" + e.Project + "\x00" + e.Revision + "\x00" + token))
	return hex.EncodeToString(sum[:])
}

func (e hubProjectEnrollment) matchesCredential(token string) bool {
	return isLowerHex64(token) && e.Credential != "" && subtle.ConstantTimeCompare([]byte(e.Credential), []byte(e.verifier(token))) == 1
}

// The attachment owner supplies the admitted project identity and plan, and
// the serving owner supplies the current routing epoch. These inputs are
// internal to the backend; supplied HTTP headers carry the credential only.
type hubProjectRebind struct {
	Group          string
	Project        string
	PriorRevision  string
	AttachmentPlan string
	Epoch          MCPFrontRoutingTargetSnapshot
}

// stageHubProjectRebind quarantines the exact group before rotating its token.
// The returned credential is for the selected backend attachment owner only.
// No scope becomes eligible here, including after an interrupted write.
type hubProjectIssuance struct{ Revision, Token, ProjectRoot, CapsuleLeaf, GroupSHA256 string }

func stageHubProjectRebind(ctx context.Context, input hubProjectRebind, issued ...hubProjectIssuance) (enrollment hubProjectEnrollment, credential string, resultErr error) {
	revision, err := generateHexToken()
	if err != nil {
		return hubProjectEnrollment{}, "", err
	}
	if len(issued) > 1 {
		return hubProjectEnrollment{}, "", errHubProjectAuthority
	}
	if len(issued) == 1 {
		if !isLowerHex64(issued[0].Revision) || !isLowerHex64(issued[0].Token) {
			return hubProjectEnrollment{}, "", errHubProjectAuthority
		}
		revision = issued[0].Revision
	}
	next := hubProjectEnrollment{Group: input.Group, Project: input.Project, Revision: revision,
		PriorRevision: input.PriorRevision, Generation: input.Epoch.Generation, Port: input.Epoch.Port, AttachmentPlan: input.AttachmentPlan}
	if len(issued) == 1 {
		next.ProjectRoot, next.CapsuleLeaf, next.GroupSHA256 = issued[0].ProjectRoot, issued[0].CapsuleLeaf, issued[0].GroupSHA256
	}
	if !next.valid() || !next.matchesEpoch(input.Epoch) {
		return hubProjectEnrollment{}, "", errHubProjectAuthority
	}
	release, err := AcquireHubMcpMutationContext(ctx)
	if err != nil {
		return hubProjectEnrollment{}, "", err
	}
	defer ReleaseAndJoin(&resultErr, release, "release project enrollment Hub lease")
	groups, err := loadGroupsLocked()
	if err != nil {
		return hubProjectEnrollment{}, "", err
	}
	declared := false
	for _, group := range groups.Groups {
		if group.Name == input.Group {
			if next.GroupSHA256 != "" {
				digest, err := projectClientValueHash(group)
				if err != nil || digest != next.GroupSHA256 {
					return hubProjectEnrollment{}, "", errHubProjectAuthority
				}
			}
			declared = true
			break
		}
	}
	if !declared {
		return hubProjectEnrollment{}, "", errHubProjectAuthority
	}
	table, err := loadHubTokensLocked()
	if err != nil {
		return hubProjectEnrollment{}, "", err
	}
	scope := GroupScopeKey(input.Group)
	priorToken, exists := table.Tokens[scope]
	if !exists {
		return hubProjectEnrollment{}, "", errHubProjectAuthority
	}
	if err := mutateHubProjectAuthority(ctx, input.Epoch, next.Group, func(prior hubProjectEnrollment) (hubProjectEnrollment, error) {
		if prior.Revision != input.PriorRevision || prior.Revision != "" &&
			(prior.Group != input.Group || !prior.Revoked && (prior.AttachmentReceipt == "" || !prior.matchesCredential(priorToken))) {
			return hubProjectEnrollment{}, errHubProjectAuthority
		}
		return next, nil
	}); err != nil {
		publishTokenTable(table)
		return hubProjectEnrollment{}, "", err
	}
	publishTokenTable(table)

	fresh, err := generateHexToken()
	if err != nil {
		return hubProjectEnrollment{}, "", err
	}
	if len(issued) == 1 {
		fresh = issued[0].Token
	}
	if subtle.ConstantTimeCompare([]byte(fresh), []byte(priorToken)) == 1 {
		return hubProjectEnrollment{}, "", errHubProjectAuthority
	}
	table.Tokens[scope] = fresh
	if err := writeHubTokensLocked(table); err != nil {
		return hubProjectEnrollment{}, "", err
	}
	staged := next
	staged.Credential = staged.verifier(fresh)
	if err := mutateHubProjectAuthority(ctx, input.Epoch, next.Group, func(prior hubProjectEnrollment) (hubProjectEnrollment, error) {
		if prior != next {
			return hubProjectEnrollment{}, errHubProjectAuthority
		}
		return staged, nil
	}); err != nil {
		return hubProjectEnrollment{}, "", err
	}
	publishTokenTable(table)
	return staged, fresh, nil
}

// This receipt is authorized by the attachment owner and live serving owner.
// Composition passes it after settled old-credential denial and the intended
// project's exclusive attachment and readiness have been received.
type hubProjectRebindReceiving struct {
	Project           string
	Revision          string
	AttachmentPlan    string
	AttachmentReceipt string
	Epoch             MCPFrontRoutingTargetSnapshot
}

func commitHubProjectRebind(ctx context.Context, staged hubProjectEnrollment, receiving hubProjectRebindReceiving) (resultErr error) {
	if staged.AttachmentReceipt != "" || staged.Credential == "" || !staged.valid() ||
		staged.Project != receiving.Project || staged.Revision != receiving.Revision ||
		staged.AttachmentPlan != receiving.AttachmentPlan || !isLowerHex64(receiving.AttachmentReceipt) ||
		!staged.matchesEpoch(receiving.Epoch) {
		return errHubProjectAuthority
	}
	release, err := AcquireHubMcpMutationContext(ctx)
	if err != nil {
		return err
	}
	defer ReleaseAndJoin(&resultErr, release, "release project commit Hub lease")
	table, err := loadHubTokensLocked()
	if err != nil {
		return err
	}
	if !staged.matchesCredential(table.Tokens[GroupScopeKey(staged.Group)]) {
		return errHubProjectAuthority
	}
	if err := mutateHubProjectAuthority(ctx, receiving.Epoch, staged.Group, func(prior hubProjectEnrollment) (hubProjectEnrollment, error) {
		if prior != staged {
			return hubProjectEnrollment{}, errHubProjectAuthority
		}
		prior.AttachmentReceipt = receiving.AttachmentReceipt
		return prior, nil
	}); err != nil {
		return err
	}
	publishTokenTable(table)
	return nil
}

// All newly connected project mutations share the existing release ledger.
// An unconfirmed release poisons this leaf for subsequent mutation attempts.
func AcquireHubMcpMutationContext(ctx context.Context) (func() error, error) {
	root, err := DaemonStateDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, hubMcpLockFileLeaf)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		release, acquired, err := tryLockLeafLedgered(path)
		if err != nil || acquired {
			return release, err
		}
		timer := time.NewTimer(hubMcpLockRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Revoke only the issuance recorded by this attachment owner. A successor
// revision is neither restored nor rotated by an old rollback.
func revokeHubProjectEnrollment(ctx context.Context, original hubProjectEnrollment, epoch MCPFrontRoutingTargetSnapshot) (resultErr error) {
	release, err := AcquireHubMcpMutationContext(ctx)
	if err != nil {
		return err
	}
	defer ReleaseAndJoin(&resultErr, release, "release project revoke Hub lease")
	table, err := loadHubTokensLocked()
	if err != nil {
		return err
	}
	if err := mutateHubProjectAuthority(ctx, epoch, original.Group, func(current hubProjectEnrollment) (hubProjectEnrollment, error) {
		if current.Revision != original.Revision || current.Project != original.Project || current.AttachmentPlan != original.AttachmentPlan {
			return hubProjectEnrollment{}, errHubProjectAuthority
		}
		current.Revoked = true
		current.AttachmentReceipt = ""
		return current, nil
	}); err != nil {
		return err
	}
	publishTokenTable(table)
	return nil
}

// Caller owns hub-mcp.lock. The routing epoch is read and matched inside the
// existing settings writer; no attachment or serving callback runs under it.
func mutateHubProjectAuthority(ctx context.Context, expected MCPFrontRoutingTargetSnapshot, group string, mutate func(hubProjectEnrollment) (hubProjectEnrollment, error)) error {
	path := SettingsPath()
	if path == "" || ctx.Err() != nil {
		return errors.Join(errHubProjectAuthority, ctx.Err())
	}
	key := hubProjectGroupKey(group)
	var committed hubProjectEnrollment
	return mutateSettingsMapLockedThen(path, readHubProjectSettingsMap, func(raw map[string]string) error {
		actual, err := mcpFrontRoutingEpochFromRaw(raw, false)
		if err != nil || actual != expected {
			return errHubProjectAuthority
		}
		record, err := hubProjectAuthorityFromSettings(raw)
		if err != nil {
			return err
		}
		committed, err = mutate(record.Groups[key])
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		record.Groups[key] = committed
		encoded, err := json.Marshal(record)
		if err != nil {
			return errHubProjectAuthority
		}
		raw[hubProjectAuthoritySettingsKey] = string(encoded)
		return nil
	}, func(raw map[string]string) error {
		record, err := hubProjectAuthorityFromSettings(raw)
		if err != nil || record.Groups[key] != committed {
			return errHubProjectAuthority
		}
		return nil
	})
}
