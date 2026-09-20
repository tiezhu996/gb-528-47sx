package dto

import (
	"encoding/json"
	"testing"

	"stage-rigging-cue-interlock/backend/internal/model"

	"gorm.io/datatypes"
)

func TestStaleDevicesDetectsVersionDrift(t *testing.T) {
	pins := []model.DevicePin{{DeviceID: 3, DeviceCode: "TRUSS-01", PinnedVersion: 2}}
	devices := []model.RiggingDevice{{ID: 3, DeviceCode: "TRUSS-01", Version: 4}}
	stale := StaleDevices(pins, []uint{3}, devices)
	if len(stale) != 1 {
		t.Fatalf("expected one stale device, got %d", len(stale))
	}
	entry := stale[0]
	if entry.PinnedVersion != 2 || entry.CurrentVersion != 4 || entry.Reason != StaleReasonVersionChanged {
		t.Fatalf("unexpected stale entry: %+v", entry)
	}
}

func TestStaleDevicesFlagsUnpinnedActionDevice(t *testing.T) {
	devices := []model.RiggingDevice{{ID: 8, DeviceCode: "LX-02", Version: 1}}
	stale := StaleDevices(nil, []uint{8}, devices)
	if len(stale) != 1 || stale[0].Reason != StaleReasonNotPinned || stale[0].PinnedVersion != 0 {
		t.Fatalf("expected unpinned device to be stale, got %+v", stale)
	}
	if fresh := StaleDevices([]model.DevicePin{{DeviceID: 8, DeviceCode: "LX-02", PinnedVersion: 1}}, []uint{8}, devices); len(fresh) != 0 {
		t.Fatalf("matching pins must not be stale, got %+v", fresh)
	}
}

func TestCueFromModelComputesStaleOnlyForReviewedStatuses(t *testing.T) {
	pinsJSON, _ := json.Marshal([]model.DevicePin{{DeviceID: 5, DeviceCode: "CLOUD-03", PinnedVersion: 1}})
	base := model.CueDefinition{CueCode: "Q-010", CueStatus: "approved", ActionsJSON: datatypes.JSON([]byte(`[{"device_id":5,"start_offset_ms":0,"duration_ms":1000,"from_position_m":1,"to_position_m":2,"load_kg":10}]`)), DependenciesJSON: datatypes.JSON([]byte(`[]`)), DevicePinsJSON: datatypes.JSON(pinsJSON)}
	devices := []model.RiggingDevice{{ID: 5, DeviceCode: "CLOUD-03", Version: 2}}
	approved, err := CueFromModel(base, devices)
	if err != nil {
		t.Fatalf("map approved cue: %v", err)
	}
	if len(approved.StaleDevices) != 1 {
		t.Fatalf("approved cue with drifted pin must be stale, got %+v", approved.StaleDevices)
	}
	base.CueStatus = "draft"
	draft, err := CueFromModel(base, devices)
	if err != nil {
		t.Fatalf("map draft cue: %v", err)
	}
	if len(draft.StaleDevices) != 0 {
		t.Fatalf("draft cues have no pins to invalidate, got %+v", draft.StaleDevices)
	}
}
