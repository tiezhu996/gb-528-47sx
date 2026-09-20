package service

import (
	"stage-rigging-cue-interlock/backend/internal/constants"
	"stage-rigging-cue-interlock/backend/internal/dto"
	"stage-rigging-cue-interlock/backend/internal/model"
	"stage-rigging-cue-interlock/backend/internal/repository"
)

const (
	deviceLockStaleReviewMessage = "One or more referenced devices changed parameters while this cue waited in review. Reject it to draft, resubmit the same content and approve again to refresh the version lock."
	deviceLockStalePinnedMessage = "One or more referenced devices changed parameters after this cue version was approved. Historical approval and rehearsal results stay available, but this cue version can no longer be locked or rehearsed. Reject it to draft, resubmit the same content and approve again to recover."
	deviceLockMissingMessage     = "This cue carries no device version lock. Reject it to draft, resubmit the same content and approve again before locking."
)

// DeviceLockEnricher joins Cue device pins with live devices for API responses.
// It never mutates state: listing a stale cue must remain a read-only view of
// the historical approval and of why it can no longer be locked or rehearsed.
type DeviceLockEnricher struct {
	pins    *repository.CueDeviceVersionRepository
	devices *repository.RiggingDeviceRepository
}

func NewDeviceLockEnricher(pins *repository.CueDeviceVersionRepository, devices *repository.RiggingDeviceRepository) *DeviceLockEnricher {
	return &DeviceLockEnricher{pins: pins, devices: devices}
}

// Apply fills device_locks and the aggregate stale/missing flags on a response.
func (e *DeviceLockEnricher) Apply(response *dto.CueDefinitionResponse, cue model.CueDefinition) error {
	pins, err := e.pins.ByCueID(cue.ID)
	if err != nil {
		return err
	}
	if len(pins) == 0 {
		response.DeviceLocks = []dto.DeviceVersionLock{}
		// Drafts legitimately have no pin; only post-review states are missing.
		if constants.CueStatus(cue.CueStatus) == constants.CueApproved || constants.CueStatus(cue.CueStatus) == constants.CueLocked {
			response.DeviceLockMissing = true
			response.DeviceLockReason = deviceLockMissingMessage
		}
		return nil
	}
	deviceIDs := make([]uint, 0, len(pins))
	for _, pin := range pins {
		deviceIDs = append(deviceIDs, pin.DeviceID)
	}
	devices, err := e.devices.ByIDs(deviceIDs)
	if err != nil {
		return err
	}
	deviceByID := make(map[uint]model.RiggingDevice, len(devices))
	for _, device := range devices {
		deviceByID[device.ID] = device
	}
	locks := make([]dto.DeviceVersionLock, 0, len(pins))
	for _, pin := range pins {
		device, found := deviceByID[pin.DeviceID]
		lock := dto.DeviceVersionLock{
			DeviceID:       pin.DeviceID,
			ReviewVersion:  pin.ReviewVersion,
			PinnedVersion:  pin.PinnedVersion,
			CurrentVersion: pin.PinnedVersion,
		}
		if found {
			lock.DeviceCode = device.DeviceCode
			lock.DeviceName = device.Name
			lock.SafetyZone = device.SafetyZone
			lock.CurrentVersion = device.Version
			lock.Stale = e.isStale(cue, pin, device)
			if lock.Stale {
				lock.StaleReason = e.reason(cue)
			}
		} else {
			lock.CurrentVersion = 0
			lock.Stale = true
			lock.StaleReason = "the referenced device was removed"
		}
		locks = append(locks, lock)
	}
	response.DeviceLocks = locks
	for _, lock := range locks {
		if lock.Stale {
			response.DeviceLockStale = true
			response.DeviceLockReason = e.reason(cue)
			break
		}
	}
	return nil
}

// isStale chooses the comparison version by cue state: pending_review is still
// protected by the review snapshot, while approved/locked rely on the approval
// pin. Draft rows should never surface because rejecting clears them.
func (e *DeviceLockEnricher) isStale(cue model.CueDefinition, pin model.CueDeviceVersion, device model.RiggingDevice) bool {
	switch constants.CueStatus(cue.CueStatus) {
	case constants.CuePendingReview:
		return device.Version != pin.ReviewVersion
	case constants.CueApproved, constants.CueLocked, constants.CueArchived:
		return device.Version != pin.PinnedVersion
	default:
		return false
	}
}

func (e *DeviceLockEnricher) reason(cue model.CueDefinition) string {
	if constants.CueStatus(cue.CueStatus) == constants.CuePendingReview {
		return deviceLockStaleReviewMessage
	}
	return deviceLockStalePinnedMessage
}
