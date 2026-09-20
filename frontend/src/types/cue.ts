export type CueStatus = 'draft' | 'pending_review' | 'approved' | 'locked' | 'archived'

export interface CueAction {
  device_id: number
  start_offset_ms: number
  duration_ms: number
  from_position_m: number
  to_position_m: number
  load_kg: number
}

export interface DeviceVersionLock {
  device_id: number
  device_code: string
  device_name: string
  safety_zone: string
  review_version: number
  pinned_version: number
  current_version: number
  stale: boolean
  stale_reason: string
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
  review_note: string
  device_locks: DeviceVersionLock[]
  device_lock_stale: boolean
  device_lock_reason: string
  device_lock_missing: boolean
  created_at: string
  updated_at: string
}

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
