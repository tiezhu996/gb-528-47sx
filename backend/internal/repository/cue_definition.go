package repository

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"stage-rigging-cue-interlock/backend/internal/audit"
	"stage-rigging-cue-interlock/backend/internal/constants"
	"stage-rigging-cue-interlock/backend/internal/model"
	"stage-rigging-cue-interlock/backend/internal/util"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type CueDefinitionRepository struct {
	db    *gorm.DB
	audit *audit.Repository
}

func NewCueDefinitionRepository(db *gorm.DB, auditRepository *audit.Repository) *CueDefinitionRepository {
	return &CueDefinitionRepository{db: db, audit: auditRepository}
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

func (r *CueDefinitionRepository) Transition(id uint, expectedVersion uint, from, to constants.CueStatus, reviewerID *uint, note string, event audit.Event) (model.CueDefinition, error) {
	var updated model.CueDefinition
	err := r.db.Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{"cue_status": to, "version": expectedVersion + 1, "review_note": note}
		if to == constants.CueApproved {
			updates["approved_by"] = reviewerID
		}
		if to == constants.CueDraft {
			updates["approved_by"] = nil
		}
		result := tx.Model(&model.CueDefinition{}).Where("id = ? AND version = ? AND cue_status = ?", id, expectedVersion, from).Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("transition cue: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return util.Conflict("CUE_VERSION_CONFLICT", "cue state or version changed concurrently", nil)
		}
		if err := r.audit.WithTx(tx).Record(event); err != nil {
			return err
		}
		if err := tx.First(&updated, id).Error; err != nil {
			return fmt.Errorf("reload transitioned cue: %w", err)
		}
		return nil
	})
	return updated, err
}

// ApproveWithDevicePins moves a pending cue to approved and atomically pins
// the current revisions of every device its actions reference. The device
// rows are locked inside the same transaction and re-verified against the
// pins the reviewer saw: if a device update committed in between, the
// approval fails with CUE_DEVICE_VERSION_DRIFT and nothing is written.
func (r *CueDefinitionRepository) ApproveWithDevicePins(id uint, expectedVersion uint, pins []model.DevicePin, reviewerID uint, note string, event audit.Event) (model.CueDefinition, error) {
	return r.writePins(id, expectedVersion, pins, reviewerID, note, event, map[string]any{"cue_status": constants.CueApproved}, []constants.CueStatus{constants.CuePendingReview})
}

// RefreshDevicePins re-approves an approved or locked cue whose pinned
// device revisions went stale. The cue content and status stay untouched;
// only the pins, review metadata, and optimistic version move forward.
func (r *CueDefinitionRepository) RefreshDevicePins(id uint, expectedVersion uint, pins []model.DevicePin, reviewerID uint, note string, event audit.Event) (model.CueDefinition, error) {
	return r.writePins(id, expectedVersion, pins, reviewerID, note, event, map[string]any{}, []constants.CueStatus{constants.CueApproved, constants.CueLocked})
}

func (r *CueDefinitionRepository) writePins(id uint, expectedVersion uint, pins []model.DevicePin, reviewerID uint, note string, event audit.Event, extra map[string]any, allowedFrom []constants.CueStatus) (model.CueDefinition, error) {
	var updated model.CueDefinition
	err := r.db.Transaction(func(tx *gorm.DB) error {
		if err := r.verifyPinsCurrent(tx, pins); err != nil {
			return err
		}
		pinsJSON, err := json.Marshal(pins)
		if err != nil {
			return fmt.Errorf("encode device pins: %w", err)
		}
		updates := map[string]any{"device_pins_json": datatypes.JSON(pinsJSON), "version": expectedVersion + 1, "approved_by": reviewerID, "review_note": note}
		for key, value := range extra {
			updates[key] = value
		}
		result := tx.Model(&model.CueDefinition{}).Where("id = ? AND version = ? AND cue_status IN ?", id, expectedVersion, allowedFrom).Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("write cue device pins: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return util.Conflict("CUE_VERSION_CONFLICT", "cue state or version changed concurrently", nil)
		}
		if err := r.audit.WithTx(tx).Record(event); err != nil {
			return err
		}
		if err := tx.First(&updated, id).Error; err != nil {
			return fmt.Errorf("reload pinned cue: %w", err)
		}
		return nil
	})
	return updated, err
}

// verifyPinsCurrent locks the referenced device rows and confirms each pin
// still matches the committed device revision. A mismatch means a device
// update won the race and the approval must be retried with fresh pins.
func (r *CueDefinitionRepository) verifyPinsCurrent(tx *gorm.DB, pins []model.DevicePin) error {
	ids := make([]uint, 0, len(pins))
	for _, pin := range pins {
		ids = append(ids, pin.DeviceID)
	}
	if err := lockRiggingDevices(tx, r.db.Dialector.Name(), ids); err != nil {
		if isLockBusy(err) {
			return util.Conflict("CUE_DEVICE_LOCK_BUSY", "device limits are being updated concurrently; retry the approval", err)
		}
		return err
	}
	devices := []model.RiggingDevice{}
	if len(ids) > 0 {
		if err := tx.Where("id IN ?", ids).Find(&devices).Error; err != nil {
			return fmt.Errorf("read locked device revisions: %w", err)
		}
	}
	current := make(map[uint]uint, len(devices))
	for _, device := range devices {
		current[device.ID] = device.Version
	}
	for _, pin := range pins {
		version, ok := current[pin.DeviceID]
		if !ok {
			return util.Unprocessable("DEVICE_REFERENCE_INVALID", "a pinned device no longer exists", map[string]any{"device_id": pin.DeviceID, "device_code": pin.DeviceCode})
		}
		if version != pin.PinnedVersion {
			return util.ConflictDetails("CUE_DEVICE_VERSION_DRIFT", "device limits changed while the approval was in progress; reload and retry", map[string]any{"device_id": pin.DeviceID, "device_code": pin.DeviceCode, "expected_version": pin.PinnedVersion, "current_version": version})
		}
	}
	return nil
}

// ApprovedWithPins lists approved or locked cues so callers can find which
// cues a device revision change invalidates.
func (r *CueDefinitionRepository) ApprovedWithPins() ([]model.CueDefinition, error) {
	var items []model.CueDefinition
	if err := r.db.Where("cue_status IN ?", []string{string(constants.CueApproved), string(constants.CueLocked)}).Order("cue_code ASC").Find(&items).Error; err != nil {
		return nil, fmt.Errorf("list approved cues with pins: %w", err)
	}
	return items, nil
}
