package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"

	"mcp-local-hub/internal/clients"
)

const projectAdoptSourcePreimageDomain = "mcphub-project-adopt-preimage-v1"

type projectAdoptSource struct {
	identity         clients.CodexProjectIdentity
	configBytes      []byte
	configSHA256     string
	logicalEntryName string
	entry            extractedStdioEntry
}

type projectAdoptRepeat struct {
	identity         clients.CodexProjectIdentity
	configSHA256     string
	logicalEntryName string
}

func extractCodexProjectAdoptSource(projectRoot, logicalEntryName string) (*projectAdoptSource, error) {
	snapshot, err := clients.ReadCodexProjectConfigSnapshot(projectRoot)
	if err != nil {
		return nil, err
	}
	configBytes := snapshot.Bytes()
	entry, err := extractCodexStdioEntryFromBytes(configBytes, logicalEntryName, codexStdioEntryProject)
	if err != nil {
		return nil, fmt.Errorf("extract Codex project source entry %q: %w", logicalEntryName, err)
	}
	return &projectAdoptSource{
		identity:         snapshot.Identity(),
		configBytes:      bytes.Clone(configBytes),
		configSHA256:     snapshot.ConfigSHA256(),
		logicalEntryName: logicalEntryName,
		entry:            entry,
	}, nil
}

func codexProjectSourcePreimageToken(projectIdentitySHA256, logicalEntryName, configSHA256 string) (string, error) {
	projectIdentity, ok := decodeLowerProjectAdoptSHA256(projectIdentitySHA256)
	if !ok {
		return "", fmt.Errorf("%w: invalid project identity fingerprint", clients.ErrCodexProjectSourceChanged)
	}
	configIdentity, ok := decodeLowerProjectAdoptSHA256(configSHA256)
	if !ok {
		return "", fmt.Errorf("%w: invalid project config fingerprint", clients.ErrCodexProjectSourceChanged)
	}
	if logicalEntryName == "" {
		return "", fmt.Errorf("%w: logical entry name is empty", clients.ErrCodexProjectSourceChanged)
	}

	material := make([]byte, 0, len(projectAdoptSourcePreimageDomain)+1+32+4+len("codex-cli")+4+len(logicalEntryName)+32)
	material = append(material, projectAdoptSourcePreimageDomain...)
	material = append(material, 0)
	material = append(material, projectIdentity...)
	var err error
	material, err = appendProjectAdoptFramedString(material, "codex-cli")
	if err != nil {
		return "", err
	}
	material, err = appendProjectAdoptFramedString(material, logicalEntryName)
	if err != nil {
		return "", err
	}
	material = append(material, configIdentity...)
	digest := sha256.Sum256(material)
	return hex.EncodeToString(digest[:]), nil
}

func appendProjectAdoptFramedString(dst []byte, value string) ([]byte, error) {
	if uint64(len(value)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("%w: framed value is too large", clients.ErrCodexProjectSourceChanged)
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	dst = append(dst, length[:]...)
	dst = append(dst, value...)
	return dst, nil
}

func decodeLowerProjectAdoptSHA256(value string) ([]byte, bool) {
	if len(value) != 64 || value != strings.ToLower(value) {
		return nil, false
	}
	decoded, err := hex.DecodeString(value)
	return decoded, err == nil && len(decoded) == sha256.Size
}
