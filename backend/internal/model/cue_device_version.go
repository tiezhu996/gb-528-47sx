package model

import "time"

// CueDeviceVersion is the per-action device version pin of a Cue.
//
// ReviewVersion is captured when the cue is submitted for review and lets the
// reviewer decision fail fast when device parameters change while the cue is
// waiting in review. PinnedVersion is the current device version at the moment
// of approval and is the "device version lock": locking and re-running the cue
// are refused once the live device version moves past it. Rows are deliberately
// kept after device changes and after archival so historical approvals and
// rehearsal evidence stay explainable.
type CueDeviceVersion struct {
	ID             uint `gorm:"primaryKey"`
	CueID          uint `gorm:"not null;uniqueIndex:uniq_cue_device_pin,priority:1;index:idx_cue_pin_cue"`
	DeviceID       uint `gorm:"not null;uniqueIndex:uniq_cue_device_pin,priority:2;index:idx_cue_pin_device"`
	ReviewVersion  uint `gorm:"not null"`
	PinnedVersion  uint `gorm:"not null;default:0"`
	ActionOrder    int  `gorm:"not null;default:0"`
	ApprovalLocked bool `gorm:"not null;default:false;index:idx_cue_pin_locked"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (CueDeviceVersion) TableName() string { return "cue_device_versions" }
