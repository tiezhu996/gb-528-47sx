export type CueStatus = 'draft' | 'pending_review' | 'approved' | 'locked' | 'archived'

export interface CueAction {
  device_id: number
  start_offset_ms: number
  duration_ms: number
  from_position_m: number
  to_position_m: number
  load_kg: number
}

export interface DevicePin {
  device_id: number
  device_code: string
  pinned_version: number
}

export type StaleDeviceReason = 'device_version_changed' | 'device_not_pinned'

export interface StaleDevice {
  device_id: number
  device_code: string
  pinned_version: number
  current_version: number
  reason: StaleDeviceReason
}

export interface CueDefinition {
  id: number
  cue_code: string
  name: string
  sequence_no: number
  start_offset_ms: number
  duration_ms: number
  cue_status: CueStatus
  version: number
  created_by: number
  approved_by: number | null
  actions: CueAction[]
  dependency_ids: number[]
  device_pins: DevicePin[]
  stale_devices: StaleDevice[]
  review_note: string
  created_at: string
  updated_at: string
}

export const isDeviceStale = (cue: CueDefinition): boolean => (cue.stale_devices ?? []).length > 0

export interface CreateCueInput {
  cue_code: string
  name: string
  sequence_no: number
  start_offset_ms: number
  duration_ms: number
  actions: CueAction[]
  dependency_ids: number[]
}

export type UpdateCueInput = Omit<CreateCueInput, 'cue_code'> & { version: number }
