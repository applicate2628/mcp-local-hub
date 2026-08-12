package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

const (
	HubEnrollmentMaxFrameBytes  = 4096
	HubEnrollmentStateIssued    = "ISSUED"
	HubEnrollmentStateEnrolled  = "ENROLLED"
	HubEnrollmentStateConsumed  = "CONSUMED"
	HubEnrollmentStateCancelled = "CANCELLED"
)

type HubEnrollmentEnrollV1 struct {
	Version          int    `json:"version"`
	Op               string `json:"op"`
	Challenge        string `json:"challenge"`
	Correlation      string `json:"correlation"`
	Task             string `json:"task"`
	Generation       int    `json:"generation"`
	CapabilitySHA256 string `json:"capability_sha256"`
}

type HubEnrollmentCancelV1 struct {
	Version     int    `json:"version"`
	Op          string `json:"op"`
	Correlation string `json:"correlation"`
}

type HubEnrollmentReceiptV1 struct {
	Version        int    `json:"version"`
	Correlation    string `json:"correlation"`
	State          string `json:"state"`
	ChannelSettled bool   `json:"channel_settled"`
}

type HubEnrollmentClientV1 interface {
	Enroll(context.Context, HubEnrollmentEnrollV1) (HubEnrollmentReceiptV1, error)
	Cancel(context.Context, HubEnrollmentCancelV1) (HubEnrollmentReceiptV1, error)
}

func ValidateHubEnrollmentEnrollV1(v HubEnrollmentEnrollV1) error {
	if v.Version != 1 || v.Op != "enroll" || v.Challenge == "" || v.Correlation == "" || v.Task != SupervisorCstTaskV1 || v.Generation <= 0 {
		return fmt.Errorf("invalid enrollment request")
	}
	digest, err := hex.DecodeString(v.CapabilitySHA256)
	if err != nil || len(digest) != 32 {
		return fmt.Errorf("invalid enrollment capability digest")
	}
	return nil
}

func ValidateHubEnrollmentCancelV1(v HubEnrollmentCancelV1) error {
	if v.Version != 1 || v.Op != "cancel" || v.Correlation == "" {
		return fmt.Errorf("invalid enrollment cancellation")
	}
	return nil
}

func ValidateHubEnrollmentReceiptV1(v HubEnrollmentReceiptV1, correlation, state string) error {
	if v.Version != 1 || correlation == "" || v.Correlation != correlation || v.State != state || !v.ChannelSettled {
		return fmt.Errorf("invalid enrollment receipt")
	}
	return nil
}

func DecodeHubEnrollmentFrameV1(raw []byte, dst any) error {
	if len(raw) == 0 || len(raw) > HubEnrollmentMaxFrameBytes {
		return fmt.Errorf("enrollment frame size invalid")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("enrollment frame has trailing data")
	}
	return nil
}
