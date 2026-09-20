package repository

import (
	"errors"
	"fmt"
	"strings"

	"stage-rigging-cue-interlock/backend/internal/audit"
	"stage-rigging-cue-interlock/backend/internal/constants"
	"stage-rigging-cue-interlock/backend/internal/model"
	"stage-rigging-cue-interlock/backend/internal/util"

	"gorm.io/gorm"
)

type CueDefinitionRepository struct {
	db    *gorm.DB
	audit *audit.Repository
	pins  *CueDeviceVersionRepository
}

func NewCueDefinitionRepository(db *gorm.DB, auditRepository *audit.Repository, pins *CueDeviceVersionRepository) *CueDefinitionRepository {
	return &CueDefinitionRepository{db: db, audit: auditRepository, pins: pins}
}

func (r *CueDefinitionRepository) List(page, pageSize int, status, search string) ([]model.CueDefinition, int64, error) {
	query := r.db.Model(&model.CueDefinition{})
	if status != "" {
		query = query.Where("cue_status = ?", status)
	}
	if search != "" {
		term := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(cue_code) LIKE ? OR LOWER(name) LIKE ?", term, term)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count cue definitions: %w", err)
	}
	var items []model.CueDefinition
	if err := query.Order("sequence_no ASC, cue_code ASC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error; err != nil {
		return nil, 0, fmt.Errorf("list cue definitions: %w", err)
	}
	return items, total, nil
}

func (r *CueDefinitionRepository) Get(id uint) (model.CueDefinition, error) {
	var item model.CueDefinition
	if err := r.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.CueDefinition{}, util.NotFound("CUE_NOT_FOUND", "cue definition was not found")
		}
		return model.CueDefinition{}, fmt.Errorf("get cue definition: %w", err)
	}
	return item, nil
}

func (r *CueDefinitionRepository) ByIDs(ids []uint) ([]model.CueDefinition, error) {
	var items []model.CueDefinition
	if err := r.db.Where("id IN ?", ids).Order("sequence_no ASC, cue_code ASC").Find(&items).Error; err != nil {
		return nil, fmt.Errorf("get cues by ids: %w", err)
	}
	if len(items) != len(uniqueIDs(ids)) {
		return nil, util.Unprocessable("CUE_SET_INVALID", "one or more selected cues do not exist", map[string]any{"requested_ids": ids, "found_count": len(items)})
	}
	return items, nil
}

func (r *CueDefinitionRepository) Create(item *model.CueDefinition, event audit.Event) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(item).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return util.Conflict("CUE_CODE_CONFLICT", "cue_code already exists", err)
			}
			return fmt.Errorf("create cue definition: %w", err)
		}
		event.EntityID = item.ID
		return r.audit.WithTx(tx).Record(event)
	})
}

func (r *CueDefinitionRepository) Update(item *model.CueDefinition, expectedVersion uint, event audit.Event) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{"name": item.Name, "sequence_no": item.SequenceNo, "start_offset_ms": item.StartOffsetMS, "duration_ms": item.DurationMS, "actions_json": item.ActionsJSON, "dependencies_json": item.DependenciesJSON, "version": expectedVersion + 1}
		result := tx.Model(&model.CueDefinition{}).Where("id = ? AND version = ? AND cue_status = ?", item.ID, expectedVersion, constants.CueDraft).Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("update draft cue: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return util.Conflict("CUE_VERSION_CONFLICT", "cue changed or is no longer editable as a draft", nil)
		}
		if err := r.audit.WithTx(tx).Record(event); err != nil {
			return err
		}
		item.Version = expectedVersion + 1
		return nil
	})
}

// SubmitForReview moves draft -> pending_review and replaces the device pins
// with the current device versions referenced by the submitted actions.
func (r *CueDefinitionRepository) SubmitForReview(id, expectedVersion uint, note string, event audit.Event) (model.CueDefinition, error) {
	var updated model.CueDefinition
	err := r.db.Transaction(func(tx *gorm.DB) error {
		if err := lockCueRow(tx, id); err != nil {
			return err
		}
		var cue model.CueDefinition
		if err := tx.First(&cue, id).Error; err != nil {
			return fmt.Errorf("load cue for submit: %w", err)
		}
		if cue.CueStatus != string(constants.CueDraft) || cue.Version != expectedVersion {
			return util.Conflict("CUE_VERSION_CONFLICT", "cue state or version changed concurrently", nil)
		}
		actionDeviceIDs, err := actionDeviceIDs(tx, id)
		if err != nil {
			return err
		}
		if err := lockDeviceRows(tx, actionDeviceIDs); err != nil {
			return err
		}
		versions, err := deviceVersionMap(tx, actionDeviceIDs)
		if err != nil {
			return err
		}
		if err := r.pins.ReplaceForReview(tx, id, actionDeviceIDs, versions); err != nil {
			return err
		}
		result := tx.Model(&model.CueDefinition{}).
			Where("id = ? AND version = ? AND cue_status = ?", id, expectedVersion, constants.CueDraft).
			Updates(map[string]any{"cue_status": constants.CuePendingReview, "version": expectedVersion + 1, "review_note": note})
		if result.Error != nil {
			return fmt.Errorf("submit cue for review: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return util.Conflict("CUE_VERSION_CONFLICT", "cue state or version changed concurrently", nil)
		}
		if err := r.audit.WithTx(tx).Record(event); err != nil {
			return err
		}
		return tx.First(&updated, id).Error
	})
	return updated, err
}

// Approve moves pending_review -> approved and freezes the device version lock.
// Referenced device rows are locked first and re-read; a version different from
// the review snapshot fails with CUE_DEVICE_VERSION_STALE and rolls everything
// back. The shared keyed locker makes the same race fail in-process against a
// concurrent device parameter update: one side acquires, the other gets 409.
func (r *CueDefinitionRepository) Approve(id, expectedVersion uint, reviewerID uint, note string, event audit.Event) (model.CueDefinition, error) {
	var updated model.CueDefinition
	preload, err := r.Get(id)
	if err != nil {
		return model.CueDefinition{}, err
	}
	if preload.CueStatus != string(constants.CuePendingReview) || preload.Version != expectedVersion {
		return model.CueDefinition{}, util.Conflict("CUE_VERSION_CONFLICT", "cue state or version changed concurrently", nil)
	}
	preloadPins, err := r.pins.ByCueID(id)
	if err != nil {
		return model.CueDefinition{}, err
	}
	if len(preloadPins) == 0 {
		return model.CueDefinition{}, util.Unprocessable("CUE_DEVICE_LOCK_MISSING", "cue has no review-time device pin; return it to draft and resubmit", map[string]any{"cue_id": id})
	}
	keys := make([]uint64, 0, len(preloadPins))
	for _, pin := range preloadPins {
		keys = append(keys, uint64(pin.DeviceID))
	}
	acquired, ok := r.pins.locker.AcquireOrdered(keys)
	if !ok {
		return model.CueDefinition{}, util.ConflictDetails("DEVICE_UPDATE_REVIEW_CONFLICT", "a referenced device is being updated; reload and approve again once the device update finishes", map[string]any{"cue_id": id})
	}
	defer r.pins.locker.ReleaseOrdered(acquired)
	err = r.db.Transaction(func(tx *gorm.DB) error {
		if err := lockCueRow(tx, id); err != nil {
			return err
		}
		var cue model.CueDefinition
		if err := tx.First(&cue, id).Error; err != nil {
			return fmt.Errorf("load cue for approval: %w", err)
		}
		if cue.CueStatus != string(constants.CuePendingReview) || cue.Version != expectedVersion {
			return util.Conflict("CUE_VERSION_CONFLICT", "cue state or version changed concurrently", nil)
		}
		pins, err := r.pins.ByCueIDTx(tx, id)
		if err != nil {
			return err
		}
		if len(pins) == 0 {
			return util.Unprocessable("CUE_DEVICE_LOCK_MISSING", "cue has no review-time device pin; return it to draft and resubmit", map[string]any{"cue_id": id})
		}
		deviceIDs := make([]uint, 0, len(pins))
		for _, pin := range pins {
			deviceIDs = append(deviceIDs, pin.DeviceID)
		}
		if err := lockDeviceRows(tx, deviceIDs); err != nil {
			return err
		}
		currentVersions, devices, err := devicesForPins(tx, pins)
		if err != nil {
			return err
		}
		if stale := buildDeviceStaleDetails(pins, devices, "review"); len(stale) > 0 {
			return util.ConflictDetails("CUE_DEVICE_VERSION_STALE", "device parameters changed while the cue waited in review; approve the same content again after resubmission", map[string]any{"cue_id": cue.ID, "cue_code": cue.CueCode, "cue_version": cue.Version, "cue_status": cue.CueStatus, "phase": StalePhaseReview, "reason": staleReason(StalePhaseReview), "stale_devices": stale})
		}
		if _, err := r.pins.LockForApproval(tx, id, currentVersions); err != nil {
			return err
		}
		result := tx.Model(&model.CueDefinition{}).
			Where("id = ? AND version = ? AND cue_status = ?", id, expectedVersion, constants.CuePendingReview).
			Updates(map[string]any{"cue_status": constants.CueApproved, "version": expectedVersion + 1, "approved_by": reviewerID, "review_note": note})
		if result.Error != nil {
			return fmt.Errorf("approve cue: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return util.Conflict("CUE_VERSION_CONFLICT", "cue state or version changed concurrently", nil)
		}
		if err := r.audit.WithTx(tx).Record(event); err != nil {
			return err
		}
		return tx.First(&updated, id).Error
	})
	return updated, err
}

// ReturnToDraft moves pending_review or approved back to draft and clears pins.
// A stale approved Cue must travel this path before it can be re-approved with
// fresh device versions (same content, new review cycle).
func (r *CueDefinitionRepository) ReturnToDraft(id, expectedVersion uint, from constants.CueStatus, note string, event audit.Event) (model.CueDefinition, error) {
	var updated model.CueDefinition
	err := r.db.Transaction(func(tx *gorm.DB) error {
		if err := lockCueRow(tx, id); err != nil {
			return err
		}
		result := tx.Model(&model.CueDefinition{}).
			Where("id = ? AND version = ? AND cue_status = ?", id, expectedVersion, from).
			Updates(map[string]any{"cue_status": constants.CueDraft, "version": expectedVersion + 1, "approved_by": nil, "review_note": note})
		if result.Error != nil {
			return fmt.Errorf("return cue to draft: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return util.Conflict("CUE_VERSION_CONFLICT", "cue state or version changed concurrently", nil)
		}
		if err := r.pins.ClearForCue(tx, id); err != nil {
			return err
		}
		if err := r.audit.WithTx(tx).Record(event); err != nil {
			return err
		}
		return tx.First(&updated, id).Error
	})
	return updated, err
}

// LockVersion moves approved -> locked only when every approval pin still
// matches the live device version. Stale pins abort with the affected devices
// and old/new versions and nothing is updated.
func (r *CueDefinitionRepository) LockVersion(id, expectedVersion uint, note string, event audit.Event) (model.CueDefinition, error) {
	var updated model.CueDefinition
	err := r.db.Transaction(func(tx *gorm.DB) error {
		if err := lockCueRow(tx, id); err != nil {
			return err
		}
		var cue model.CueDefinition
		if err := tx.First(&cue, id).Error; err != nil {
			return fmt.Errorf("load cue for lock: %w", err)
		}
		if cue.CueStatus != string(constants.CueApproved) || cue.Version != expectedVersion {
			return util.Conflict("CUE_VERSION_CONFLICT", "cue state or version changed concurrently", nil)
		}
		pins, err := r.pins.ByCueIDTx(tx, id)
		if err != nil {
			return err
		}
		if len(pins) == 0 {
			return util.Unprocessable("CUE_DEVICE_LOCK_MISSING", "approved cue has no device version lock; return it to draft, resubmit and approve again", map[string]any{"cue_id": id})
		}
		deviceIDs := make([]uint, 0, len(pins))
		for _, pin := range pins {
			deviceIDs = append(deviceIDs, pin.DeviceID)
		}
		if err := lockDeviceRows(tx, deviceIDs); err != nil {
			return err
		}
		var devices []model.RiggingDevice
		if err := tx.Where("id IN ?", deviceIDs).Find(&devices).Error; err != nil {
			return fmt.Errorf("load pinned devices for lock: %w", err)
		}
		if stale := buildDeviceStaleDetails(pins, devices, "pinned"); len(stale) > 0 {
			return util.ConflictDetails("CUE_DEVICE_VERSION_STALE", "the cue cannot be locked because referenced devices changed after approval", map[string]any{"cue_id": id, "phase": StalePhasePinned, "reason": staleReason(StalePhasePinned), "stale_devices": stale})
		}
		result := tx.Model(&model.CueDefinition{}).
			Where("id = ? AND version = ? AND cue_status = ?", id, expectedVersion, constants.CueApproved).
			Updates(map[string]any{"cue_status": constants.CueLocked, "version": expectedVersion + 1, "review_note": note})
		if result.Error != nil {
			return fmt.Errorf("lock cue version: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return util.Conflict("CUE_VERSION_CONFLICT", "cue state or version changed concurrently", nil)
		}
		if err := r.audit.WithTx(tx).Record(event); err != nil {
			return err
		}
		return tx.First(&updated, id).Error
	})
	return updated, err
}

// Archive moves locked -> archived. Pins are retained as historical evidence.
func (r *CueDefinitionRepository) Archive(id, expectedVersion uint, note string, event audit.Event) (model.CueDefinition, error) {
	var updated model.CueDefinition
	err := r.db.Transaction(func(tx *gorm.DB) error {
		if err := lockCueRow(tx, id); err != nil {
			return err
		}
		result := tx.Model(&model.CueDefinition{}).
			Where("id = ? AND version = ? AND cue_status = ?", id, expectedVersion, constants.CueLocked).
			Updates(map[string]any{"cue_status": constants.CueArchived, "version": expectedVersion + 1, "review_note": note})
		if result.Error != nil {
			return fmt.Errorf("archive cue: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return util.Conflict("CUE_VERSION_CONFLICT", "cue state or version changed concurrently", nil)
		}
		if err := r.audit.WithTx(tx).Record(event); err != nil {
			return err
		}
		return tx.First(&updated, id).Error
	})
	return updated, err
}

// actionDeviceIDs reads the ordered, de-duplicated device IDs referenced by the
// cue actions JSON inside the supplied transaction.
func actionDeviceIDs(tx *gorm.DB, cueID uint) ([]uint, error) {
	var cue model.CueDefinition
	if err := tx.Select("actions_json").First(&cue, cueID).Error; err != nil {
		return nil, fmt.Errorf("load cue actions for device pins: %w", err)
	}
	return deviceIDsFromActionsJSON(cue.ActionsJSON)
}

func deviceVersionMap(tx *gorm.DB, deviceIDs []uint) (map[uint]uint, error) {
	var devices []model.RiggingDevice
	if err := tx.Where("id IN ?", deviceIDs).Find(&devices).Error; err != nil {
		return nil, fmt.Errorf("read device versions: %w", err)
	}
	versions := make(map[uint]uint, len(devices))
	for _, device := range devices {
		versions[device.ID] = device.Version
	}
	if len(versions) != len(uniqueIDs(deviceIDs)) {
		return nil, util.Unprocessable("DEVICE_REFERENCE_INVALID", "one or more referenced devices do not exist", map[string]any{"requested_ids": deviceIDs})
	}
	return versions, nil
}

func devicesForPins(tx *gorm.DB, pins []model.CueDeviceVersion) (map[uint]uint, []model.RiggingDevice, error) {
	deviceIDs := make([]uint, 0, len(pins))
	for _, pin := range pins {
		deviceIDs = append(deviceIDs, pin.DeviceID)
	}
	var devices []model.RiggingDevice
	if err := tx.Where("id IN ?", deviceIDs).Find(&devices).Error; err != nil {
		return nil, nil, fmt.Errorf("read pinned devices: %w", err)
	}
	versions := make(map[uint]uint, len(devices))
	for _, device := range devices {
		versions[device.ID] = device.Version
	}
	if len(versions) != len(uniqueIDs(deviceIDs)) {
		return nil, nil, util.Unprocessable("DEVICE_REFERENCE_INVALID", "one or more pinned devices do not exist", map[string]any{"requested_ids": deviceIDs})
	}
	return versions, devices, nil
}
