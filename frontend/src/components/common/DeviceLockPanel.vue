<script setup lang="ts">
import { computed } from 'vue'
import { AlertTriangle, ShieldCheck } from 'lucide-vue-next'
import type { CueDefinition, DeviceVersionLock } from '../../types/cue'

const props = defineProps<{ cue: CueDefinition }>()

const staleLocks = computed(() => props.cue.device_locks.filter((lock) => lock.stale))
const currentLocks = computed(() => props.cue.device_locks.filter((lock) => !lock.stale))

const phaseLabel = computed(() => {
  if (props.cue.cue_status === 'pending_review') return 'review snapshot'
  if (props.cue.cue_status === 'approved') return 'approval pin'
  return 'locked pin'
})

function rowClass({ row }: { row: DeviceVersionLock }): string {
  return row.stale ? 'stale-lock-row' : ''
}
</script>

<template>
  <div class="device-lock-panel" :class="{ stale: cue.device_lock_stale }">
    <div class="device-lock-heading">
      <p class="eyebrow">DEVICE VERSION LOCK · {{ phaseLabel }}</p>
      <el-tag v-if="cue.device_locks.length === 0" type="info" effect="plain" size="small">No device pins yet</el-tag>
      <el-tag v-else-if="cue.device_lock_stale" type="danger" effect="dark" size="small">
        <AlertTriangle :size="13" /> {{ staleLocks.length }} stale device(s)
      </el-tag>
      <el-tag v-else type="success" effect="plain" size="small">
        <ShieldCheck :size="13" /> All device versions current
      </el-tag>
    </div>
    <el-alert
      v-if="cue.device_lock_stale"
      class="device-lock-alert"
      type="error"
      :closable="false"
      show-icon
      :title="cue.device_lock_reason || 'Referenced devices changed parameters. Re-approve the same cue content to recover.'"
    />
    <el-alert
      v-else-if="cue.device_lock_missing"
      class="device-lock-alert"
      type="warning"
      :closable="false"
      show-icon
      title="This cue has no device version lock. Return it to draft, resubmit and approve again before locking."
    />
    <el-table v-if="cue.device_locks.length > 0" :data="cue.device_locks" size="small" :row-class-name="rowClass">
      <el-table-column label="Device" min-width="150">
        <template #default="scope">
          <strong>{{ scope.row.device_code || `DEVICE-${scope.row.device_id}` }}</strong>
          <div class="subtle">{{ scope.row.device_name || 'device removed' }}</div>
        </template>
      </el-table-column>
      <el-table-column label="Review v" width="82" prop="review_version" />
      <el-table-column label="Approved v" width="92" prop="pinned_version" />
      <el-table-column label="Live v" width="76">
        <template #default="scope">
          <span :class="{ 'version-mismatch': scope.row.stale }">{{ scope.row.current_version || '—' }}</span>
        </template>
      </el-table-column>
      <el-table-column label="State" width="120">
        <template #default="scope">
          <el-tag v-if="scope.row.stale" type="danger" size="small" effect="plain">Invalidated</el-tag>
          <el-tag v-else type="success" size="small" effect="plain">Current</el-tag>
        </template>
      </el-table-column>
    </el-table>
    <p v-if="staleLocks.length > 0" class="device-lock-hint">
      Historical approval and rehearsal results are retained. Locking and new rehearsal runs for this cue
      version are blocked until a reviewer returns it to draft, the same content is resubmitted and approved again.
    </p>
    <p v-else-if="currentLocks.length > 0" class="device-lock-hint subtle">
      Pins record the device parameter versions at approval. Rehearsal evidence is immutable; changing a
      device afterwards invalidates the pin but never overwrites prior runs.
    </p>
  </div>
</template>

<style scoped>
.device-lock-panel {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  padding: 10px 12px;
  background: var(--el-fill-color-blank);
}
.device-lock-panel.stale {
  border-color: var(--el-color-danger-light-5);
  background: var(--el-color-danger-light-9);
}
.device-lock-heading {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-bottom: 8px;
}
.device-lock-alert {
  margin: 6px 0 8px;
}
.device-lock-hint {
  margin: 8px 0 0;
  font-size: 12px;
  line-height: 1.5;
  color: var(--el-text-color-secondary);
}
.version-mismatch {
  color: var(--el-color-danger);
  font-weight: 700;
}
:deep(.stale-lock-row) {
  background: var(--el-color-danger-light-9);
}
</style>
