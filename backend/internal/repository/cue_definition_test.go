package repository

import (
	"encoding/json"
	"errors"
	"testing"

	"stage-rigging-cue-interlock/backend/internal/audit"
	"stage-rigging-cue-interlock/backend/internal/constants"
	"stage-rigging-cue-interlock/backend/internal/model"
	"stage-rigging-cue-interlock/backend/internal/util"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openPinTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if err := db.AutoMigrate(&model.RiggingDevice{}, &model.CueDefinition{}, &audit.Event{}); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return db
}

func seedPinTestData(t *testing.T, db *gorm.DB) (model.RiggingDevice, model.CueDefinition) {
	t.Helper()
	device := model.RiggingDevice{DeviceCode: "TRUSS-01", Name: "Test truss", DeviceType: "motorized_batten", MaxLoadKG: 500, MaxSpeedMS: 0.5, TravelMinM: 4, TravelMaxM: 16, SafetyZone: "overstage-a", DeviceStatus: "available", Version: 1}
	if err := db.Create(&device).Error; err != nil {
		t.Fatalf("create device: %v", err)
	}
	actions, _ := json.Marshal([]map[string]any{{"device_id": device.ID, "start_offset_ms": 0, "duration_ms": 1000, "from_position_m": 10, "to_position_m": 8, "load_kg": 200}})
	cue := model.CueDefinition{CueCode: "Q-900", Name: "Pin test cue", SequenceNo: 900, StartOffsetMS: 0, DurationMS: 2000, CueStatus: string(constants.CuePendingReview), Version: 1, CreatedBy: 1, ActionsJSON: datatypes.JSON(actions), DependenciesJSON: datatypes.JSON([]byte("[]")), DevicePinsJSON: datatypes.JSON([]byte("[]"))}
	if err := db.Create(&cue).Error; err != nil {
		t.Fatalf("create cue: %v", err)
	}
	return device, cue
}

func pinEvent() audit.Event {
	return audit.Event{RequestID: "test-request", ActorID: 1, ActorUsername: "reviewer", Action: "cue_definition.approve", EntityType: "cue_definition"}
}

func TestApproveWithDevicePinsCapturesRevisions(t *testing.T) {
	db := openPinTestDB(t)
	device, cue := seedPinTestData(t, db)
	repo := NewCueDefinitionRepository(db, audit.NewRepository(db))
	pins := []model.DevicePin{{DeviceID: device.ID, DeviceCode: device.DeviceCode, PinnedVersion: device.Version}}

	updated, err := repo.ApproveWithDevicePins(cue.ID, 1, pins, 7, "approved for rehearsal planning", pinEvent())
	if err != nil {
		t.Fatalf("approve with pins: %v", err)
	}
	if updated.CueStatus != string(constants.CueApproved) || updated.Version != 2 {
		t.Fatalf("unexpected cue state after approval: status=%s version=%d", updated.CueStatus, updated.Version)
	}
	stored, err := decodePins(updated)
	if err != nil {
		t.Fatalf("decode stored pins: %v", err)
	}
	if len(stored) != 1 || stored[0].PinnedVersion != 1 || stored[0].DeviceCode != "TRUSS-01" {
		t.Fatalf("stored pins do not match approval revisions: %+v", stored)
	}
	var events []audit.Event
	if err := db.Find(&events).Error; err != nil || len(events) != 1 {
		t.Fatalf("approval must record one audit event, got %d (%v)", len(events), err)
	}
}

func TestApproveWithDevicePinsRejectsDriftedRevisions(t *testing.T) {
	db := openPinTestDB(t)
	device, cue := seedPinTestData(t, db)
	repo := NewCueDefinitionRepository(db, audit.NewRepository(db))
	if err := db.Model(&model.RiggingDevice{}).Where("id = ?", device.ID).Update("version", 2).Error; err != nil {
		t.Fatalf("bump device version: %v", err)
	}
	stalePins := []model.DevicePin{{DeviceID: device.ID, DeviceCode: device.DeviceCode, PinnedVersion: 1}}

	_, err := repo.ApproveWithDevicePins(cue.ID, 1, stalePins, 7, "racing approval", pinEvent())
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != "CUE_DEVICE_VERSION_DRIFT" {
		t.Fatalf("expected CUE_DEVICE_VERSION_DRIFT, got %v", err)
	}
	var reloaded model.CueDefinition
	if err := db.First(&reloaded, cue.ID).Error; err != nil {
		t.Fatalf("reload cue: %v", err)
	}
	if reloaded.CueStatus != string(constants.CuePendingReview) || reloaded.Version != 1 {
		t.Fatalf("failed approval must not half-update the cue: status=%s version=%d", reloaded.CueStatus, reloaded.Version)
	}
	if string(reloaded.DevicePinsJSON) != "[]" {
		t.Fatalf("failed approval must not write pins, got %s", reloaded.DevicePinsJSON)
	}
	var eventCount int64
	db.Model(&audit.Event{}).Count(&eventCount)
	if eventCount != 0 {
		t.Fatalf("failed approval must not write audit events, got %d", eventCount)
	}
}

func TestRefreshDevicePinsRestoresStaleCue(t *testing.T) {
	db := openPinTestDB(t)
	device, cue := seedPinTestData(t, db)
	repo := NewCueDefinitionRepository(db, audit.NewRepository(db))
	pins := []model.DevicePin{{DeviceID: device.ID, DeviceCode: device.DeviceCode, PinnedVersion: 1}}
	approved, err := repo.ApproveWithDevicePins(cue.ID, 1, pins, 7, "initial approval", pinEvent())
	if err != nil {
		t.Fatalf("approve with pins: %v", err)
	}
	if err := db.Model(&model.RiggingDevice{}).Where("id = ?", device.ID).Update("version", 2).Error; err != nil {
		t.Fatalf("bump device version: %v", err)
	}

	if _, err := repo.RefreshDevicePins(cue.ID, approved.Version, pins, 7, "stale re-approval", pinEvent()); err == nil {
		t.Fatal("re-approval with superseded pins must fail")
	}
	freshPins := []model.DevicePin{{DeviceID: device.ID, DeviceCode: device.DeviceCode, PinnedVersion: 2}}
	refreshed, err := repo.RefreshDevicePins(cue.ID, approved.Version, freshPins, 9, "re-approved against device revision 2", pinEvent())
	if err != nil {
		t.Fatalf("refresh pins: %v", err)
	}
	if refreshed.CueStatus != string(constants.CueApproved) || refreshed.Version != 3 || refreshed.ApprovedBy == nil || *refreshed.ApprovedBy != 9 {
		t.Fatalf("unexpected refreshed cue: status=%s version=%d approved_by=%v", refreshed.CueStatus, refreshed.Version, refreshed.ApprovedBy)
	}
	stored, err := decodePins(refreshed)
	if err != nil {
		t.Fatalf("decode refreshed pins: %v", err)
	}
	if len(stored) != 1 || stored[0].PinnedVersion != 2 {
		t.Fatalf("refreshed pins must pin revision 2, got %+v", stored)
	}
}

func decodePins(cue model.CueDefinition) ([]model.DevicePin, error) {
	pins := []model.DevicePin{}
	if err := json.Unmarshal(cue.DevicePinsJSON, &pins); err != nil {
		return nil, err
	}
	return pins, nil
}
