package repository

import (
	"encoding/json"
	"fmt"

	"stage-rigging-cue-interlock/backend/internal/dto"
	"stage-rigging-cue-interlock/backend/internal/model"
)

// StaleDeviceDetail is the single source of truth for the payload carried by
// CUE_DEVICE_VERSION_STALE errors. The API contract states the affected device
// and both the frozen (review or approval) version and the live version.
type StaleDeviceDetail struct {
	DeviceID       uint   `json:"device_id"`
	DeviceCode     string `json:"device_code"`
	DeviceName     string `json:"device_name"`
	LockedVersion  uint   `json:"locked_version"`
	CurrentVersion uint   `json:"current_version"`
	InvalidatedAt  string `json:"phase"`
	Reason         string `json:"reason"`
	SafetyZone     string `json:"safety_zone"`
}

// CueStaleDetail groups invalidated devices under the Cue whose version lock
// moved out of date. Rehearsal runs collect one entry per affected Cue.
type CueStaleDetail struct {
	CueID        uint                `json:"cue_id"`
	CueCode      string              `json:"cue_code"`
	CueVersion   uint                `json:"cue_version"`
	CueStatus    string              `json:"cue_status"`
	StaleDevices []StaleDeviceDetail `json:"stale_devices"`
	Reason       string              `json:"reason"`
}

const (
	// StalePhaseReview compares against the review snapshot: a device changed
	// while the Cue waited for approval.
	StalePhaseReview = "review"
	// StalePhasePinned compares against the approval pin: a device changed
	// after approval, so lock/rehearsal must be refused.
	StalePhasePinned = "pinned"
)

func staleReason(phase string) string {
	if phase == StalePhaseReview {
		return "device parameters changed while this cue waited in review"
	}
	return "device parameters changed after this cue version was approved"
}

// buildDeviceStaleDetails returns one entry per pin whose frozen version no
// longer matches the live device. An empty slice means the lock is current.
func buildDeviceStaleDetails(pins []model.CueDeviceVersion, devices []model.RiggingDevice, phase string) []StaleDeviceDetail {
	byID := make(map[uint]model.RiggingDevice, len(devices))
	for _, device := range devices {
		byID[device.ID] = device
	}
	details := make([]StaleDeviceDetail, 0)
	for _, pin := range pins {
		device, ok := byID[pin.DeviceID]
		if !ok {
			details = append(details, StaleDeviceDetail{DeviceID: pin.DeviceID, LockedVersion: frozenVersion(pin, phase), CurrentVersion: 0, InvalidatedAt: phase, Reason: "the referenced device no longer exists"})
			continue
		}
		frozen := frozenVersion(pin, phase)
		if device.Version == frozen {
			continue
		}
		details = append(details, StaleDeviceDetail{
			DeviceID:       device.ID,
			DeviceCode:     device.DeviceCode,
			DeviceName:     device.Name,
			LockedVersion:  frozen,
			CurrentVersion: device.Version,
			InvalidatedAt:  phase,
			Reason:         staleReason(phase),
			SafetyZone:     device.SafetyZone,
		})
	}
	return details
}

func frozenVersion(pin model.CueDeviceVersion, phase string) uint {
	if phase == StalePhaseReview {
		return pin.ReviewVersion
	}
	return pin.PinnedVersion
}

// AggregateCueStaleness evaluates approval pins for a set of (typically locked)
// Cues against live devices and returns one CueStaleDetail per invalidated Cue.
// Cues without any pin are reported separately so callers can refuse rehearsal
// deterministically instead of silently treating the lock as current.
func AggregateCueStaleness(cues []model.CueDefinition, pins []model.CueDeviceVersion, devices []model.RiggingDevice) (stale []CueStaleDetail, missing []uint) {
	deviceByID := make(map[uint]model.RiggingDevice, len(devices))
	for _, device := range devices {
		deviceByID[device.ID] = device
	}
	pinsByCue := make(map[uint][]model.CueDeviceVersion)
	for _, pin := range pins {
		pinsByCue[pin.CueID] = append(pinsByCue[pin.CueID], pin)
	}
	stale = make([]CueStaleDetail, 0)
	missing = make([]uint, 0)
	for _, cue := range cues {
		cuePins, ok := pinsByCue[cue.ID]
		if !ok || len(cuePins) == 0 {
			missing = append(missing, cue.ID)
			continue
		}
		cueDevices := make([]model.RiggingDevice, 0, len(cuePins))
		for _, pin := range cuePins {
			if device, found := deviceByID[pin.DeviceID]; found {
				cueDevices = append(cueDevices, device)
			}
		}
		// Pins whose device vanished are absent from cueDevices, so the builder
		// reports them with current_version 0; every version mismatch is included.
		if details := buildDeviceStaleDetails(cuePins, cueDevices, StalePhasePinned); len(details) > 0 {
			stale = append(stale, CueStaleDetail{CueID: cue.ID, CueCode: cue.CueCode, CueVersion: cue.Version, CueStatus: cue.CueStatus, StaleDevices: details, Reason: staleReason(StalePhasePinned)})
		}
	}
	return stale, missing
}

// deviceIDsFromActionsJSON decodes the Cue action snapshot and returns the
// ordered unique device list that the version pin set must cover.
func deviceIDsFromActionsJSON(raw []byte) ([]uint, error) {
	var actions []dto.CueAction
	if err := json.Unmarshal(raw, &actions); err != nil {
		return nil, fmt.Errorf("decode cue actions for device pins: %w", err)
	}
	ids := make([]uint, 0, len(actions))
	seen := make(map[uint]struct{}, len(actions))
	for _, action := range actions {
		if _, ok := seen[action.DeviceID]; ok {
			continue
		}
		seen[action.DeviceID] = struct{}{}
		ids = append(ids, action.DeviceID)
	}
	return ids, nil
}
