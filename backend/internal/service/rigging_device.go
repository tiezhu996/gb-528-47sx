package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"stage-rigging-cue-interlock/backend/internal/audit"
	"stage-rigging-cue-interlock/backend/internal/dto"
	"stage-rigging-cue-interlock/backend/internal/model"
	"stage-rigging-cue-interlock/backend/internal/repository"
	"stage-rigging-cue-interlock/backend/internal/util"
)

type RiggingDeviceService struct {
	devices *repository.RiggingDeviceRepository
	rules   *repository.InterlockRuleRepository
	pins    *repository.CueDeviceVersionRepository
	cues    *repository.CueDefinitionRepository
}

func NewRiggingDeviceService(devices *repository.RiggingDeviceRepository, rules *repository.InterlockRuleRepository, pins *repository.CueDeviceVersionRepository, cues *repository.CueDefinitionRepository) *RiggingDeviceService {
	return &RiggingDeviceService{devices: devices, rules: rules, pins: pins, cues: cues}
}

func (s *RiggingDeviceService) List(page, pageSize int, status, search string) ([]dto.RiggingDeviceResponse, int64, error) {
	items, total, err := s.devices.List(page, pageSize, status, search)
	if err != nil {
		return nil, 0, err
	}
	responses := make([]dto.RiggingDeviceResponse, 0, len(items))
	for _, item := range items {
		response, mapErr := s.withRules(item)
		if mapErr != nil {
			return nil, 0, mapErr
		}
		responses = append(responses, response)
	}
	return responses, total, nil
}

func (s *RiggingDeviceService) Get(id uint) (dto.RiggingDeviceResponse, error) {
	item, err := s.devices.Get(id)
	if err != nil {
		return dto.RiggingDeviceResponse{}, err
	}
	return s.withRules(item)
}

func (s *RiggingDeviceService) Create(request dto.CreateRiggingDeviceRequest, actor audit.ActorContext) (dto.RiggingDeviceResponse, error) {
	if err := validateDeviceEnvelope(request.TravelMinM, request.TravelMaxM); err != nil {
		return dto.RiggingDeviceResponse{}, err
	}
	item := model.RiggingDevice{DeviceCode: normalizeCode(request.DeviceCode), Name: strings.TrimSpace(request.Name), DeviceType: request.DeviceType, MaxLoadKG: request.MaxLoadKG, MaxSpeedMS: request.MaxSpeedMS, TravelMinM: request.TravelMinM, TravelMaxM: request.TravelMaxM, SafetyZone: strings.ToLower(strings.TrimSpace(request.SafetyZone)), DeviceStatus: request.DeviceStatus, Version: 1}
	after := util.SummaryJSON(map[string]any{"device_code": item.DeviceCode, "limits": map[string]any{"max_load_kg": item.MaxLoadKG, "max_speed_ms": item.MaxSpeedMS, "travel_min_m": item.TravelMinM, "travel_max_m": item.TravelMaxM}, "safety_zone": item.SafetyZone, "status": item.DeviceStatus, "version": item.Version})
	if err := s.devices.Create(&item, audit.NewEvent(actor, "rigging_device.create", "rigging_device", 0, "{}", after)); err != nil {
		return dto.RiggingDeviceResponse{}, err
	}
	return s.withRules(item)
}

// Update applies the version-checked parameter change and, after commit,
// appends an audit entry per approved/locked Cue whose device version lock the
// update invalidated. The Cues themselves are not modified: historical
// approvals and rehearsal runs must remain exactly as they were.
func (s *RiggingDeviceService) Update(id uint, request dto.UpdateRiggingDeviceRequest, actor audit.ActorContext) (dto.RiggingDeviceResponse, error) {
	if err := validateDeviceEnvelope(request.TravelMinM, request.TravelMaxM); err != nil {
		return dto.RiggingDeviceResponse{}, err
	}
	current, err := s.devices.Get(id)
	if err != nil {
		return dto.RiggingDeviceResponse{}, err
	}
	changed, before, after := s.changeSummary(current, request)
	current.Name = strings.TrimSpace(request.Name)
	current.DeviceType = request.DeviceType
	current.MaxLoadKG = request.MaxLoadKG
	current.MaxSpeedMS = request.MaxSpeedMS
	current.TravelMinM = request.TravelMinM
	current.TravelMaxM = request.TravelMaxM
	current.SafetyZone = strings.ToLower(strings.TrimSpace(request.SafetyZone))
	current.DeviceStatus = request.DeviceStatus
	if err := s.devices.Update(&current, request.Version, audit.NewEvent(actor, "rigging_device.update_limits", "rigging_device", id, before, after)); err != nil {
		return dto.RiggingDeviceResponse{}, err
	}
	// Parameters only move from version N to N+1 here, so pins with an older
	// version become stale at this exact commit. The invalidation evidence is
	// recorded as its own append-only event per affected Cue.
	if changed {
		if err := s.recordInvalidatedLocks(id, request.Version+1, actor); err != nil {
			return dto.RiggingDeviceResponse{}, err
		}
	}
	return s.withRules(current)
}

// recordInvalidatedLocks enumerates approval-locked pins on the updated device
// that belonged to approved/locked Cues and writes one audit event per Cue.
// Pins from pending_review Cues also go stale there, so they are included to
// give the reviewer the full blast radius; each event carries old/new versions.
func (s *RiggingDeviceService) recordInvalidatedLocks(deviceID, newVersion uint, actor audit.ActorContext) error {
	pins, err := s.pins.ActiveByDevice(deviceID)
	if err != nil {
		return err
	}
	// Include review-phase pins so the reviewer sees the conflict that will
	// surface as CUE_DEVICE_VERSION_STALE on the approve action.
	reviewPins, err := s.pins.ReviewPinsByDevice(deviceID)
	if err != nil {
		return err
	}
	byCue := map[uint]uint{}
	for _, pin := range append(pins, reviewPins...) {
		// Prefer the approval-pinned version when both exist for the same Cue.
		if pin.PinnedVersion > 0 {
			byCue[pin.CueID] = pin.PinnedVersion
		} else if _, exists := byCue[pin.CueID]; !exists {
			byCue[pin.CueID] = pin.ReviewVersion
		}
	}
	if len(byCue) == 0 {
		return nil
	}
	cueIDs := make([]uint, 0, len(byCue))
	for cueID := range byCue {
		cueIDs = append(cueIDs, cueID)
	}
	cues, err := s.cues.ByIDs(cueIDs)
	if err != nil {
		return err
	}
	device, err := s.devices.Get(deviceID)
	if err != nil {
		return err
	}
	for _, cue := range cues {
		oldVersion := byCue[cue.ID]
		summary := util.SummaryJSON(map[string]any{
			"device_id": deviceID, "device_code": device.DeviceCode, "device_version_old": oldVersion, "device_version_new": newVersion,
			"cue_id": cue.ID, "cue_code": cue.CueCode, "cue_version": cue.Version, "cue_status": cue.CueStatus,
			"effect":   "cue device version lock invalidated; historical approval and rehearsal runs retained; lock/rehearsal blocked until same content is re-approved",
			"boundary": "offline rehearsal evidence only; no machinery command or operational clearance",
		})
		event := audit.NewEvent(actor, "rigging_device.invalidates_cue_lock", "cue_definition", cue.ID, "{}", summary)
		if err := s.devices.RecordAudit(event); err != nil {
			return err
		}
	}
	return nil
}

func (s *RiggingDeviceService) changeSummary(current model.RiggingDevice, request dto.UpdateRiggingDeviceRequest) (bool, string, string) {
	before := util.SummaryJSON(map[string]any{"limits": map[string]any{"max_load_kg": current.MaxLoadKG, "max_speed_ms": current.MaxSpeedMS, "travel_min_m": current.TravelMinM, "travel_max_m": current.TravelMaxM}, "safety_zone": current.SafetyZone, "status": current.DeviceStatus, "version": current.Version})
	after := util.SummaryJSON(map[string]any{"limits": map[string]any{"max_load_kg": request.MaxLoadKG, "max_speed_ms": request.MaxSpeedMS, "travel_min_m": request.TravelMinM, "travel_max_m": request.TravelMaxM}, "safety_zone": strings.ToLower(strings.TrimSpace(request.SafetyZone)), "status": request.DeviceStatus, "version": request.Version + 1})
	changed := current.MaxLoadKG != request.MaxLoadKG || current.MaxSpeedMS != request.MaxSpeedMS || current.TravelMinM != request.TravelMinM || current.TravelMaxM != request.TravelMaxM || current.SafetyZone != strings.ToLower(strings.TrimSpace(request.SafetyZone)) || current.DeviceStatus != request.DeviceStatus
	return changed, before, after
}

func (s *RiggingDeviceService) withRules(item model.RiggingDevice) (dto.RiggingDeviceResponse, error) {
	response := dto.RiggingDeviceFromModel(item)
	rules, _, err := s.rules.List(1, 200, "", "", "")
	if err != nil {
		return dto.RiggingDeviceResponse{}, err
	}
	for _, rule := range rules {
		ids := []uint{}
		if err := json.Unmarshal(rule.DeviceIDsJSON, &ids); err != nil {
			return dto.RiggingDeviceResponse{}, fmt.Errorf("decode rule %s device scope: %w", rule.RuleCode, err)
		}
		for _, ruleDeviceID := range ids {
			if ruleDeviceID == item.ID {
				response.ApplicableRules = append(response.ApplicableRules, dto.RuleReference{ID: rule.ID, RuleCode: rule.RuleCode, RuleType: rule.RuleType, Severity: rule.Severity, Enabled: rule.Enabled, RuleVersion: rule.RuleVersion})
				break
			}
		}
	}
	return response, nil
}

func validateDeviceEnvelope(minimum, maximum float64) error {
	if maximum <= minimum {
		return util.Unprocessable("DEVICE_TRAVEL_INVALID", "travel_max_m must be greater than travel_min_m", map[string]any{"travel_min_m": minimum, "travel_max_m": maximum})
	}
	return nil
}

func normalizeCode(value string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), " ", "-"))
}
