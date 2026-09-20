package repository

import (
	"errors"
	"fmt"
	"strings"

	"stage-rigging-cue-interlock/backend/internal/audit"
	"stage-rigging-cue-interlock/backend/internal/concurrency"
	"stage-rigging-cue-interlock/backend/internal/model"
	"stage-rigging-cue-interlock/backend/internal/util"

	"gorm.io/gorm"
)

type RiggingDeviceRepository struct {
	db     *gorm.DB
	audit  *audit.Repository
	locker *concurrency.KeyedLocker
}

func NewRiggingDeviceRepository(db *gorm.DB, auditRepository *audit.Repository, locker *concurrency.KeyedLocker) *RiggingDeviceRepository {
	return &RiggingDeviceRepository{db: db, audit: auditRepository, locker: locker}
}

func (r *RiggingDeviceRepository) List(page, pageSize int, status, search string) ([]model.RiggingDevice, int64, error) {
	query := r.db.Model(&model.RiggingDevice{})
	if status != "" {
		query = query.Where("device_status = ?", status)
	}
	if search != "" {
		term := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(device_code) LIKE ? OR LOWER(name) LIKE ? OR LOWER(safety_zone) LIKE ?", term, term, term)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count rigging devices: %w", err)
	}
	var items []model.RiggingDevice
	if err := query.Order("device_code ASC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error; err != nil {
		return nil, 0, fmt.Errorf("list rigging devices: %w", err)
	}
	return items, total, nil
}

func (r *RiggingDeviceRepository) All() ([]model.RiggingDevice, error) {
	var items []model.RiggingDevice
	if err := r.db.Order("device_code ASC").Find(&items).Error; err != nil {
		return nil, fmt.Errorf("list all rigging devices: %w", err)
	}
	return items, nil
}

func (r *RiggingDeviceRepository) Get(id uint) (model.RiggingDevice, error) {
	var item model.RiggingDevice
	if err := r.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.RiggingDevice{}, util.NotFound("DEVICE_NOT_FOUND", "rigging device was not found")
		}
		return model.RiggingDevice{}, fmt.Errorf("get rigging device: %w", err)
	}
	return item, nil
}

func (r *RiggingDeviceRepository) ByIDs(ids []uint) ([]model.RiggingDevice, error) {
	if len(ids) == 0 {
		return []model.RiggingDevice{}, nil
	}
	var items []model.RiggingDevice
	if err := r.db.Where("id IN ?", ids).Find(&items).Error; err != nil {
		return nil, fmt.Errorf("get rigging devices by ids: %w", err)
	}
	if len(items) != len(uniqueIDs(ids)) {
		return nil, util.Unprocessable("DEVICE_REFERENCE_INVALID", "one or more referenced devices do not exist", map[string]any{"requested_ids": ids, "found_count": len(items)})
	}
	return items, nil
}

func (r *RiggingDeviceRepository) Create(item *model.RiggingDevice, event audit.Event) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(item).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return util.Conflict("DEVICE_CODE_CONFLICT", "device_code already exists", err)
			}
			return fmt.Errorf("create rigging device: %w", err)
		}
		event.EntityID = item.ID
		if err := r.audit.WithTx(tx).Record(event); err != nil {
			return err
		}
		return nil
	})
}

// Update changes device parameters under the same per-device lock that guards
// Cue approval. When a Cue approval touching this device is already in flight
// the TryLock fails and the service rejects the update with a 409 so that only
// one of the two operations can commit. The optimistic version predicate makes
// a lost update between reading the form and saving impossible, and the row
// lock inside the transaction closes the cross-process window.
func (r *RiggingDeviceRepository) Update(item *model.RiggingDevice, expectedVersion uint, event audit.Event) error {
	key := uint64(item.ID)
	if !r.locker.TryLock(key) {
		return util.ConflictDetails("DEVICE_UPDATE_REVIEW_CONFLICT", "device parameters cannot change while a cue referencing the device is being approved", map[string]any{"device_id": item.ID})
	}
	defer r.locker.Unlock(key)
	return r.db.Transaction(func(tx *gorm.DB) error {
		var locked model.RiggingDevice
		lockQuery := tx.Model(&model.RiggingDevice{}).Where("id = ? AND version = ?", item.ID, expectedVersion)
		if tx.Dialector.Name() != "sqlite" {
			lockQuery = lockQuery.Clauses(forUpdateClause())
		}
		if err := lockQuery.Find(&locked).Error; err != nil {
			return fmt.Errorf("lock rigging device for update: %w", err)
		}
		if locked.ID == 0 {
			var current model.RiggingDevice
			if findErr := tx.Select("version").First(&current, item.ID).Error; findErr != nil {
				if errors.Is(findErr, gorm.ErrRecordNotFound) {
					return util.NotFound("DEVICE_NOT_FOUND", "rigging device was not found")
				}
				return fmt.Errorf("re-read rigging device version: %w", findErr)
			}
			return util.ConflictDetails("DEVICE_VERSION_CONFLICT", "device limits changed since they were loaded", map[string]any{"expected_version": expectedVersion, "current_version": current.Version})
		}
		updates := map[string]any{"name": item.Name, "device_type": item.DeviceType, "max_load_kg": item.MaxLoadKG, "max_speed_ms": item.MaxSpeedMS, "travel_min_m": item.TravelMinM, "travel_max_m": item.TravelMaxM, "safety_zone": item.SafetyZone, "device_status": item.DeviceStatus, "version": expectedVersion + 1}
		result := tx.Model(&model.RiggingDevice{}).Where("id = ? AND version = ?", item.ID, expectedVersion).Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("update rigging device: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return util.Conflict("DEVICE_VERSION_CONFLICT", "device limits changed since they were loaded", nil)
		}
		if err := r.audit.WithTx(tx).Record(event); err != nil {
			return err
		}
		item.Version = expectedVersion + 1
		return nil
	})
}

func uniqueIDs(ids []uint) map[uint]struct{} {
	result := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		result[id] = struct{}{}
	}
	return result
}

// RecordAudit appends an audit event outside the device update transaction
// (after its commit) for invalidated Cue version locks.
func (r *RiggingDeviceRepository) RecordAudit(event audit.Event) error {
	if err := r.audit.Record(event); err != nil {
		return err
	}
	return nil
}
