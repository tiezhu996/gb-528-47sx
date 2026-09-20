package repository

import (
	"errors"
	"fmt"
	"strings"

	"stage-rigging-cue-interlock/backend/internal/audit"
	"stage-rigging-cue-interlock/backend/internal/model"
	"stage-rigging-cue-interlock/backend/internal/util"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RiggingDeviceRepository struct {
	db    *gorm.DB
	audit *audit.Repository
}

func NewRiggingDeviceRepository(db *gorm.DB, auditRepository *audit.Repository) *RiggingDeviceRepository {
	return &RiggingDeviceRepository{db: db, audit: auditRepository}
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

func (r *RiggingDeviceRepository) Update(item *model.RiggingDevice, expectedVersion uint, event audit.Event) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := lockRiggingDevices(tx, r.db.Dialector.Name(), []uint{item.ID}); err != nil {
			if isLockBusy(err) {
				return util.Conflict("DEVICE_UPDATE_BUSY", "device limits are locked by a concurrent cue approval; retry the update", err)
			}
			return err
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

// lockRiggingDevices takes row-level locks on the given devices inside tx so
// a device limit update and a cue approval cannot interleave halfway. On
// PostgreSQL the lock is acquired with NOWAIT: a concurrent holder makes the
// caller fail fast with a conflict instead of waiting and committing on top
// of a half-visible change. SQLite serializes writers through its single
// connection, so no locking clause is emitted there.
func lockRiggingDevices(tx *gorm.DB, dialect string, ids []uint) error {
	if len(ids) == 0 || dialect != "postgres" {
		return nil
	}
	ordered := append([]uint(nil), ids...)
	for index := 1; index < len(ordered); index++ {
		for cursor := index; cursor > 0 && ordered[cursor] < ordered[cursor-1]; cursor-- {
			ordered[cursor], ordered[cursor-1] = ordered[cursor-1], ordered[cursor]
		}
	}
	var locked []uint
	if err := tx.Model(&model.RiggingDevice{}).Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"}).Where("id IN ?", ordered).Order("id ASC").Pluck("id", &locked).Error; err != nil {
		return fmt.Errorf("lock rigging devices for update: %w", err)
	}
	if len(locked) != len(uniqueIDs(ids)) {
		return util.Unprocessable("DEVICE_REFERENCE_INVALID", "one or more referenced devices do not exist", map[string]any{"requested_ids": ids, "found_count": len(locked)})
	}
	return nil
}

// isLockBusy reports whether err is PostgreSQL's lock_not_available (55P03)
// raised by a NOWAIT row lock contending with an in-flight transaction.
func isLockBusy(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "55P03"
	}
	return false
}

func uniqueIDs(ids []uint) map[uint]struct{} {
	result := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		result[id] = struct{}{}
	}
	return result
}
