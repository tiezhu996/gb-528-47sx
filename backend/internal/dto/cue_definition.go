package dto

import (
	"encoding/json"
	"fmt"
	"time"

	"stage-rigging-cue-interlock/backend/internal/constants"
	"stage-rigging-cue-interlock/backend/internal/model"
)

type CueAction struct {
	DeviceID      uint    `json:"device_id" binding:"required,gte=1"`
	StartOffsetMS int64   `json:"start_offset_ms" binding:"gte=0,lte=86400000"`
	DurationMS    int64   `json:"duration_ms" binding:"required,gt=0,lte=86400000"`
	FromPositionM float64 `json:"from_position_m" binding:"gte=-20,lte=200"`
	ToPositionM   float64 `json:"to_position_m" binding:"gte=-20,lte=200"`
	LoadKG        float64 `json:"load_kg" binding:"gte=0,lte=100000"`
}

type CreateCueRequest struct {
	CueCode       string      `json:"cue_code" binding:"required,min=2,max=48"`
	Name          string      `json:"name" binding:"required,min=3,max=120"`
	SequenceNo    int         `json:"sequence_no" binding:"required,gte=1,lte=100000"`
	StartOffsetMS int64       `json:"start_offset_ms" binding:"gte=0,lte=86400000"`
	DurationMS    int64       `json:"duration_ms" binding:"required,gt=0,lte=86400000"`
	Actions       []CueAction `json:"actions" binding:"required,min=1,max=24,dive"`
	DependencyIDs []uint      `json:"dependency_ids" binding:"max=40,dive,gte=1"`
}

type UpdateCueRequest struct {
	Name          string      `json:"name" binding:"required,min=3,max=120"`
	SequenceNo    int         `json:"sequence_no" binding:"required,gte=1,lte=100000"`
	StartOffsetMS int64       `json:"start_offset_ms" binding:"gte=0,lte=86400000"`
	DurationMS    int64       `json:"duration_ms" binding:"required,gt=0,lte=86400000"`
	Actions       []CueAction `json:"actions" binding:"required,min=1,max=24,dive"`
	DependencyIDs []uint      `json:"dependency_ids" binding:"max=40,dive,gte=1"`
	Version       uint        `json:"version" binding:"required,gte=1"`
}

type CueTransitionRequest struct {
	Version uint   `json:"version" binding:"required,gte=1"`
	Reason  string `json:"reason" binding:"required,min=4,max=500"`
}

// StaleDevice describes one referenced rigging device whose current revision
// no longer matches the version pinned when the cue was approved.
type StaleDevice struct {
	DeviceID       uint   `json:"device_id"`
	DeviceCode     string `json:"device_code"`
	PinnedVersion  uint   `json:"pinned_version"`
	CurrentVersion uint   `json:"current_version"`
	Reason         string `json:"reason"`
}

const (
	StaleReasonVersionChanged = "device_version_changed"
	StaleReasonNotPinned      = "device_not_pinned"
)

type CueDefinitionResponse struct {
	ID            uint                `json:"id"`
	CueCode       string              `json:"cue_code"`
	Name          string              `json:"name"`
	SequenceNo    int                 `json:"sequence_no"`
	StartOffsetMS int64               `json:"start_offset_ms"`
	DurationMS    int64               `json:"duration_ms"`
	CueStatus     constants.CueStatus `json:"cue_status"`
	Version       uint                `json:"version"`
	CreatedBy     uint                `json:"created_by"`
	ApprovedBy    *uint               `json:"approved_by"`
	Actions       []CueAction         `json:"actions"`
	DependencyIDs []uint              `json:"dependency_ids"`
	DevicePins    []model.DevicePin   `json:"device_pins"`
	StaleDevices  []StaleDevice       `json:"stale_devices"`
	ReviewNote    string              `json:"review_note"`
	CreatedAt     time.Time           `json:"created_at"`
	UpdatedAt     time.Time           `json:"updated_at"`
}

// DecodeDevicePins reads the approval-time device version pins stored on a
// cue. Missing pins decode to an empty list rather than an error so legacy
// rows remain loadable and surface as unpinned through StaleDevices.
func DecodeDevicePins(item model.CueDefinition) ([]model.DevicePin, error) {
	pins := []model.DevicePin{}
	if len(item.DevicePinsJSON) == 0 {
		return pins, nil
	}
	if err := json.Unmarshal(item.DevicePinsJSON, &pins); err != nil {
		return nil, fmt.Errorf("decode cue %s device pins: %w", item.CueCode, err)
	}
	return pins, nil
}

// StaleDevices compares approval pins with the current device revisions.
// A pinned device whose version moved, or an action device that was never
// pinned, makes the cue ineligible for locking or rehearsal until it is
// re-approved against the current revisions.
func StaleDevices(pins []model.DevicePin, actionDeviceIDs []uint, devices []model.RiggingDevice) []StaleDevice {
	stale := []StaleDevice{}
	if len(devices) == 0 {
		return stale
	}
	current := make(map[uint]model.RiggingDevice, len(devices))
	for _, device := range devices {
		current[device.ID] = device
	}
	pinned := make(map[uint]model.DevicePin, len(pins))
	for _, pin := range pins {
		pinned[pin.DeviceID] = pin
		device, ok := current[pin.DeviceID]
		if !ok || device.Version == pin.PinnedVersion {
			continue
		}
		stale = append(stale, StaleDevice{DeviceID: pin.DeviceID, DeviceCode: pin.DeviceCode, PinnedVersion: pin.PinnedVersion, CurrentVersion: device.Version, Reason: StaleReasonVersionChanged})
	}
	for _, deviceID := range actionDeviceIDs {
		if _, ok := pinned[deviceID]; ok {
			continue
		}
		device, ok := current[deviceID]
		if !ok {
			continue
		}
		stale = append(stale, StaleDevice{DeviceID: deviceID, DeviceCode: device.DeviceCode, PinnedVersion: 0, CurrentVersion: device.Version, Reason: StaleReasonNotPinned})
	}
	return stale
}

func CueFromModel(item model.CueDefinition, devices []model.RiggingDevice) (CueDefinitionResponse, error) {
	actions := []CueAction{}
	if err := json.Unmarshal(item.ActionsJSON, &actions); err != nil {
		return CueDefinitionResponse{}, fmt.Errorf("decode cue %s actions: %w", item.CueCode, err)
	}
	dependencies := []uint{}
	if err := json.Unmarshal(item.DependenciesJSON, &dependencies); err != nil {
		return CueDefinitionResponse{}, fmt.Errorf("decode cue %s dependencies: %w", item.CueCode, err)
	}
	pins, err := DecodeDevicePins(item)
	if err != nil {
		return CueDefinitionResponse{}, err
	}
	stale := []StaleDevice{}
	status := constants.CueStatus(item.CueStatus)
	if status == constants.CueApproved || status == constants.CueLocked {
		stale = StaleDevices(pins, ActionDeviceIDs(actions), devices)
	}
	return CueDefinitionResponse{ID: item.ID, CueCode: item.CueCode, Name: item.Name, SequenceNo: item.SequenceNo, StartOffsetMS: item.StartOffsetMS, DurationMS: item.DurationMS, CueStatus: status, Version: item.Version, CreatedBy: item.CreatedBy, ApprovedBy: item.ApprovedBy, Actions: actions, DependencyIDs: dependencies, DevicePins: pins, StaleDevices: stale, ReviewNote: item.ReviewNote, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}, nil
}

// ActionDeviceIDs returns the unique device references of a cue action list
// in first-use order.
func ActionDeviceIDs(actions []CueAction) []uint {
	seen := map[uint]bool{}
	ids := make([]uint, 0, len(actions))
	for _, action := range actions {
		if !seen[action.DeviceID] {
			seen[action.DeviceID] = true
			ids = append(ids, action.DeviceID)
		}
	}
	return ids
}
