package service

import (
	"stage-rigging-cue-interlock/backend/internal/model"
	"stage-rigging-cue-interlock/backend/internal/repository"
	"stage-rigging-cue-interlock/backend/internal/util"
)

// RehearsalDeviceLockGuard refuses a new deterministic run when any selected
// locked Cue's approval pin no longer matches the live device version. Existing
// runs are immutable and remain readable; only new derivation is blocked.
type RehearsalDeviceLockGuard struct {
	pins *repository.CueDeviceVersionRepository
}

func NewRehearsalDeviceLockGuard(pins *repository.CueDeviceVersionRepository) *RehearsalDeviceLockGuard {
	return &RehearsalDeviceLockGuard{pins: pins}
}

// EnsureCurrent aggregates every stale or lock-missing selected Cue and returns
// one 409 describing the full blast radius instead of failing one Cue at a time.
func (g *RehearsalDeviceLockGuard) EnsureCurrent(cues []model.CueDefinition, devices []model.RiggingDevice) error {
	cueIDs := make([]uint, 0, len(cues))
	for _, cue := range cues {
		cueIDs = append(cueIDs, cue.ID)
	}
	pins, err := g.pins.ByCueIDs(cueIDs)
	if err != nil {
		return err
	}
	stale, missing := repository.AggregateCueStaleness(cues, pins, devices)
	if len(stale) == 0 && len(missing) == 0 {
		return nil
	}
	missingCodes := make([]map[string]any, 0, len(missing))
	for _, cueID := range missing {
		for _, cue := range cues {
			if cue.ID == cueID {
				missingCodes = append(missingCodes, map[string]any{"cue_id": cue.ID, "cue_code": cue.CueCode, "cue_version": cue.Version, "cue_status": cue.CueStatus})
			}
		}
	}
	return util.ConflictDetails("CUE_DEVICE_VERSION_STALE",
		"one or more selected locked cues reference devices that changed parameters after approval; historical runs remain available, but this set cannot be rehearsed again until the same content is re-approved",
		map[string]any{"stale_cues": stale, "missing_lock_cues": missingCodes})
}
