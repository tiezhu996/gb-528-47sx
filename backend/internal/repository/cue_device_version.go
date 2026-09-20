package repository

import (
	"fmt"

	"stage-rigging-cue-interlock/backend/internal/concurrency"
	"stage-rigging-cue-interlock/backend/internal/constants"
	"stage-rigging-cue-interlock/backend/internal/model"
	"stage-rigging-cue-interlock/backend/internal/util"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CueDeviceVersionRepository owns the device version pins attached to Cues.
//
// Pins are only ever read or mutated together with the owning Cue transition
// inside one database transaction, so every write method here accepts the
// transaction handle supplied by the Cue repository. The keyed locker is shared
// with the device repository to serialize approvals against parameter updates
// in-process; row locks provide the same guarantee across processes.
type CueDeviceVersionRepository struct {
	db     *gorm.DB
	locker *concurrency.KeyedLocker
}

func NewCueDeviceVersionRepository(db *gorm.DB, locker *concurrency.KeyedLocker) *CueDeviceVersionRepository {
	return &CueDeviceVersionRepository{db: db, locker: locker}
}

func (r *CueDeviceVersionRepository) ByCueID(id uint) ([]model.CueDeviceVersion, error) {
	return r.loadByCue(r.db, id)
}

// ByCueIDTx reads pins inside an existing transaction. SQLite runs with a
// single pooled connection, so a non-tx read nested in an open transaction
// would deadlock waiting for that connection.
func (r *CueDeviceVersionRepository) ByCueIDTx(tx *gorm.DB, id uint) ([]model.CueDeviceVersion, error) {
	return r.loadByCue(tx, id)
}

func (r *CueDeviceVersionRepository) loadByCue(handle *gorm.DB, id uint) ([]model.CueDeviceVersion, error) {
	var items []model.CueDeviceVersion
	if err := handle.Where("cue_id = ?", id).Order("action_order ASC").Find(&items).Error; err != nil {
		return nil, fmt.Errorf("load cue device pins: %w", err)
	}
	return items, nil
}

func (r *CueDeviceVersionRepository) ByCueIDs(ids []uint) ([]model.CueDeviceVersion, error) {
	if len(ids) == 0 {
		return []model.CueDeviceVersion{}, nil
	}
	var items []model.CueDeviceVersion
	if err := r.db.Where("cue_id IN ?", ids).Order("cue_id ASC, action_order ASC").Find(&items).Error; err != nil {
		return nil, fmt.Errorf("load cue device pins by cue: %w", err)
	}
	return items, nil
}

// ActiveByDevice returns approval-locked pins whose Cue is approved or locked,
// i.e. the evidence that a device parameter update has invalidated.
func (r *CueDeviceVersionRepository) ActiveByDevice(deviceID uint) ([]model.CueDeviceVersion, error) {
	var items []model.CueDeviceVersion
	err := r.db.Table("cue_device_versions AS cdv").
		Joins("JOIN cue_definitions AS cd ON cd.id = cdv.cue_id").
		Where("cdv.device_id = ? AND cdv.approval_locked = ? AND cd.cue_status IN ?", deviceID, true, []string{string(constants.CueApproved), string(constants.CueLocked)}).
		Order("cdv.cue_id ASC, cdv.action_order ASC").
		Find(&items).Error
	if err != nil {
		return nil, fmt.Errorf("list invalidated cue pins: %w", err)
	}
	return items, nil
}

// ReviewPinsByDevice returns pins of Cues still in review that reference the
// device. Approval itself clears this state with CUE_DEVICE_VERSION_STALE, but
// enumerating them lets the device update write the invalidation evidence up
// front so reviewers see the affected Cue in the audit stream.
func (r *CueDeviceVersionRepository) ReviewPinsByDevice(deviceID uint) ([]model.CueDeviceVersion, error) {
	var items []model.CueDeviceVersion
	err := r.db.Table("cue_device_versions AS cdv").
		Joins("JOIN cue_definitions AS cd ON cd.id = cdv.cue_id").
		Where("cdv.device_id = ? AND cd.cue_status = ?", deviceID, string(constants.CuePendingReview)).
		Order("cdv.cue_id ASC, cdv.action_order ASC").
		Find(&items).Error
	if err != nil {
		return nil, fmt.Errorf("list review cue pins for device: %w", err)
	}
	return items, nil
}

// ReplaceForReview snapshots current device versions when a Cue enters review.
// Old pins are deleted first so a rejected-and-resubmitted Cue starts from a
// clean set; deletion and reinsertion happen in the caller's transaction.
func (r *CueDeviceVersionRepository) ReplaceForReview(tx *gorm.DB, cueID uint, actionDeviceIDs []uint, deviceVersions map[uint]uint) error {
	if err := tx.Where("cue_id = ?", cueID).Delete(&model.CueDeviceVersion{}).Error; err != nil {
		return fmt.Errorf("reset cue device pins: %w", err)
	}
	seen := map[uint]bool{}
	rows := make([]model.CueDeviceVersion, 0, len(actionDeviceIDs))
	for order, deviceID := range actionDeviceIDs {
		if seen[deviceID] {
			continue
		}
		seen[deviceID] = true
		version, ok := deviceVersions[deviceID]
		if !ok {
			return util.Unprocessable("DEVICE_REFERENCE_INVALID", "cue action references a device without a current version", map[string]any{"device_id": deviceID})
		}
		rows = append(rows, model.CueDeviceVersion{CueID: cueID, DeviceID: deviceID, ReviewVersion: version, PinnedVersion: 0, ActionOrder: order, ApprovalLocked: false})
	}
	if len(rows) > 0 {
		if err := tx.Create(&rows).Error; err != nil {
			return fmt.Errorf("insert review device pins: %w", err)
		}
	}
	return nil
}

// LockForApproval flips review pins into approval pins and freezes the current
// device versions. currentVersions must be read from device rows locked in the
// same transaction so a concurrent parameter update cannot slip in between.
func (r *CueDeviceVersionRepository) LockForApproval(tx *gorm.DB, cueID uint, currentVersions map[uint]uint) ([]model.CueDeviceVersion, error) {
	var rows []model.CueDeviceVersion
	if err := tx.Where("cue_id = ?", cueID).Order("action_order ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load pins for cue approval: %w", err)
	}
	if len(rows) == 0 {
		return nil, util.Unprocessable("CUE_DEVICE_LOCK_MISSING", "cue has no review-time device pin; return it to draft and resubmit", map[string]any{"cue_id": cueID})
	}
	for index := range rows {
		version, ok := currentVersions[rows[index].DeviceID]
		if !ok {
			return nil, util.Unprocessable("DEVICE_REFERENCE_INVALID", "a pinned device no longer exists", map[string]any{"device_id": rows[index].DeviceID})
		}
		rows[index].ReviewVersion = version
		rows[index].PinnedVersion = version
		rows[index].ApprovalLocked = true
		if err := tx.Model(&model.CueDeviceVersion{}).Where("id = ?", rows[index].ID).Updates(map[string]any{"review_version": version, "pinned_version": version, "approval_locked": true}).Error; err != nil {
			return nil, fmt.Errorf("freeze approval device pin: %w", err)
		}
	}
	return rows, nil
}

// ClearForCue removes every pin when the Cue is returned to draft. Historical
// approvals remain in the audit log and in prior RehearsalRun snapshots.
func (r *CueDeviceVersionRepository) ClearForCue(tx *gorm.DB, cueID uint) error {
	if err := tx.Where("cue_id = ?", cueID).Delete(&model.CueDeviceVersion{}).Error; err != nil {
		return fmt.Errorf("clear cue device pins: %w", err)
	}
	return nil
}

// forUpdateClause builds a SELECT ... FOR UPDATE row lock shared by the cue
// and device repositories so both serialize on the same database rows.
func forUpdateClause() clause.Expression {
	return clause.Locking{Strength: "UPDATE"}
}

// lockDeviceRows takes a row lock on the referenced devices in ascending ID
// order. PostgreSQL blocks a concurrent device UPDATE until commit; SQLite
// serializes writers with its database lock and the in-process keyed locker
// handles same-process concurrency there.
func lockDeviceRows(tx *gorm.DB, deviceIDs []uint) error {
	if len(deviceIDs) == 0 {
		return nil
	}
	query := tx.Model(&model.RiggingDevice{}).Where("id IN ?", deviceIDs)
	if tx.Dialector.Name() != "sqlite" {
		query = query.Clauses(forUpdateClause())
	}
	var locked []model.RiggingDevice
	if err := query.Find(&locked).Error; err != nil {
		return fmt.Errorf("lock referenced devices: %w", err)
	}
	if len(locked) != len(uniqueIDs(deviceIDs)) {
		return util.Unprocessable("DEVICE_REFERENCE_INVALID", "one or more referenced devices do not exist", map[string]any{"requested_ids": deviceIDs})
	}
	return nil
}

func lockCueRow(tx *gorm.DB, id uint) error {
	query := tx.Model(&model.CueDefinition{}).Where("id = ?", id)
	if tx.Dialector.Name() != "sqlite" {
		query = query.Clauses(forUpdateClause())
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return fmt.Errorf("lock cue row: %w", err)
	}
	if count == 0 {
		return util.NotFound("CUE_NOT_FOUND", "cue definition was not found")
	}
	return nil
}
