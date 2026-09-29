//go:build windows && cst_vptr_probe

package cstwatchdogwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const Task2SlotSize = 4096
const task2SlotPayloadLimit = 4032

// EncodeTask2Slot publishes one custodian-owned observation before the F3
// ready event. The host, not this slot, later adds the slot hash and F3
// correlation fields to the same typed Task2Evidence in the root evidence.
func EncodeTask2Slot(nonce [16]byte, sequence uint64, observation Task2Evidence) ([Task2SlotSize]byte, error) {
	var slot [Task2SlotSize]byte
	if sequence == 0 {
		return slot, errors.New("zero Task 2 observation sequence")
	}
	if err := validTask2(&observation, false); err != nil {
		return slot, err
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		return slot, err
	}
	if len(payload) == 0 || len(payload) > task2SlotPayloadLimit {
		return slot, errors.New("Task 2 observation payload length out of bounds")
	}
	copy(slot[0:4], "CST2")
	binary.LittleEndian.PutUint16(slot[4:6], 1)
	binary.LittleEndian.PutUint16(slot[6:8], uint16(len(payload)))
	copy(slot[8:24], nonce[:])
	binary.LittleEndian.PutUint64(slot[24:32], sequence)
	digest := sha256.Sum256(payload)
	copy(slot[32:64], digest[:])
	copy(slot[64:], payload)
	return slot, nil
}

// DecodeTask2Slot requires two stable complete copies and the exact F3
// nonce/sequence. It returns no target handle or parent settlement fact.
func DecodeTask2Slot(first, second []byte, nonce [16]byte, sequence uint64) (Task2Evidence, error) {
	if len(first) != Task2SlotSize || len(second) != Task2SlotSize || !bytes.Equal(first, second) {
		return Task2Evidence{}, errors.New("torn or wrong-sized Task 2 slot")
	}
	if string(first[0:4]) != "CST2" || binary.LittleEndian.Uint16(first[4:6]) != 1 ||
		!bytes.Equal(first[8:24], nonce[:]) || sequence == 0 || binary.LittleEndian.Uint64(first[24:32]) != sequence {
		return Task2Evidence{}, errors.New("Task 2 slot magic, version, nonce or sequence mismatch")
	}
	length := int(binary.LittleEndian.Uint16(first[6:8]))
	if length == 0 || length > task2SlotPayloadLimit {
		return Task2Evidence{}, errors.New("Task 2 slot payload length out of bounds")
	}
	payload := first[64 : 64+length]
	digest := sha256.Sum256(payload)
	if !bytes.Equal(first[32:64], digest[:]) {
		return Task2Evidence{}, errors.New("Task 2 slot payload hash mismatch")
	}
	for _, value := range first[64+length:] {
		if value != 0 {
			return Task2Evidence{}, errors.New("Task 2 slot tail nonzero")
		}
	}
	return decodeTask2Value(payload, false)
}

// decodeTask2Value is the one strict JSON syntax owner for the slot payload
// and the host-enriched Task 2 object in Evidence V1. Only the latter may
// carry the F3/slot correlation computed by the host after a matched read.
func decodeTask2Value(data []byte, requireCorrelation bool) (Task2Evidence, error) {
	var value Task2Evidence
	limit := task2SlotPayloadLimit
	if requireCorrelation {
		limit = 64 * 1024 // host-added correlation is not part of the slot
	}
	if len(data) == 0 || len(data) > limit || !utf8.Valid(data) {
		return value, errors.New("invalid Task 2 JSON size or UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Task2Evidence{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Task2Evidence{}, errors.New("trailing Task 2 JSON data")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Task2Evidence{}, err
	}
	for _, name := range []string{"barrier", "observerState", "target", "settlement"} {
		raw, present := fields[name]
		if !present || bytes.Equal(raw, []byte("null")) {
			return Task2Evidence{}, fmt.Errorf("missing Task 2 field %s", name)
		}
	}
	if raw, present := fields["debugger"]; present && bytes.Equal(raw, []byte("null")) {
		return Task2Evidence{}, errors.New("null Task 2 debugger")
	}
	for _, name := range []string{"f3NonceDigest", "f3Sequence", "slotSha256"} {
		raw, present := fields[name]
		if requireCorrelation {
			if !present || bytes.Equal(raw, []byte("null")) {
				return Task2Evidence{}, fmt.Errorf("missing host correlation %s", name)
			}
		} else if present {
			return Task2Evidence{}, fmt.Errorf("child slot carries host correlation %s", name)
		}
	}
	if err := validTask2(&value, requireCorrelation); err != nil {
		return Task2Evidence{}, err
	}
	return value, nil
}
