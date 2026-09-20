<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { CheckCircle2, Play, RefreshCw, Send, XCircle, AlertTriangle } from 'lucide-vue-next'
import { ElMessage } from 'element-plus'
import PageHeader from '../components/common/PageHeader.vue'
import TimelineTrack from '../components/common/TimelineTrack.vue'
import RuleEvidenceTable from '../components/common/RuleEvidenceTable.vue'
import { ApiError, errorMessage } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { useRehearsalRun } from '../hooks/useRehearsalRun'
import { useCueStore } from '../stores/cues'
import type { CueDefinition } from '../types/cue'
import type { RehearsalRun } from '../types/rehearsal'
import { formatTimestamp } from '../utils/timeline'

const cues = useCueStore()
const { store: runs, polling, poll } = useRehearsalRun()
const { canProgram, canReview } = useAuth()
const selectedCueIds = ref<number[]>([])
const reason = ref('Offline timing, rule evidence, and collision windows reviewed for rehearsal planning.')
const localError = ref('')
const selected = computed(() => runs.selected)

interface StaleCueInfo {
  cue_id: number
  cue_code: string
  cue_version: number
  cue_status: string
  reason: string
  stale_devices: Array<{ device_id: number; device_code: string; locked_version: number; current_version: number; reason: string }>
}

const staleRunFailure = ref<StaleCueInfo[]>([])

const runnableLockedCues = computed<CueDefinition[]>(() => cues.locked.filter((cue) => !cue.device_lock_stale))
const staleLockedCues = computed<CueDefinition[]>(() => cues.locked.filter((cue) => cue.device_lock_stale))

const statusType = (status: string) => ({ evaluated: 'primary', blocked: 'danger', pending_review: 'warning', approved_for_rehearsal: 'success', rejected: 'info' }[status] ?? 'info')

async function run() {
  localError.value = ''
  staleRunFailure.value = []
  try {
    const created = await runs.run(selectedCueIds.value)
    poll(created.id)
    ElMessage.success(`Immutable rehearsal #${created.id} created`)
  } catch (cause) {
    if (cause instanceof ApiError && cause.code === 'CUE_DEVICE_VERSION_STALE') {
      const details = cause.details as { stale_cues?: StaleCueInfo[] } | null
      staleRunFailure.value = details?.stale_cues ?? []
      await cues.load().catch(() => undefined)
    }
    localError.value = errorMessage(cause)
  }
}

async function submit() {
  if (!selected.value) return
  localError.value = ''
  try {
    await runs.submit(selected.value, reason.value)
    ElMessage.success('Rehearsal submitted to safety review')
  } catch (cause) {
    localError.value = errorMessage(cause)
  }
}

async function review(decision: 'approve' | 'reject') {
  if (!selected.value) return
  localError.value = ''
  try {
    const updated = await runs.review(selected.value, decision, reason.value)
    ElMessage.success(`Rehearsal ${updated.run_status.replaceAll('_', ' ')}`)
  } catch (cause) {
    localError.value = errorMessage(cause)
  }
}

onMounted(async () => {
  await Promise.all([cues.load(), runs.load()]).catch(() => undefined)
  selectedCueIds.value = runnableLockedCues.value.map((item) => item.id)
})
</script>

<template>
  <PageHeader eyebrow="DETERMINISTIC OFFLINE RUN" title="Rehearsal evidence" description="Expand locked Cue versions into one immutable timeline, then inspect every warning, blocker, and collision interval before human review.">
    <el-button :icon="RefreshCw" :loading="runs.loading" @click="runs.load">Refresh</el-button>
  </PageHeader>
  <el-alert v-if="runs.error || cues.error || localError" :title="localError || runs.error || cues.error" type="error" :closable="false" show-icon />
  <el-alert
    v-if="staleLockedCues.length > 0"
    class="stale-locked-alert"
    type="warning"
    :closable="false"
    show-icon
    title="Some locked cue versions reference changed devices and cannot be rehearsed"
  >
    <template #default>
      <p v-for="cue in staleLockedCues" :key="cue.id" class="stale-cue-line">
        <AlertTriangle :size="13" />
        <strong>{{ cue.cue_code }} v{{ cue.version }}</strong> — {{ cue.device_lock_reason }}
        <span v-for="lock in cue.device_locks.filter((item) => item.stale)" :key="lock.device_id" class="stale-device-chip">
          {{ lock.device_code }}: v{{ lock.pinned_version }} → v{{ lock.current_version }}
        </span>
      </p>
    </template>
  </el-alert>
  <section class="run-launcher">
    <div><p class="eyebrow">LOCKED INPUT SET</p><h2>Select cue versions</h2><p>{{ runnableLockedCues.length }} runnable locked cues{{ staleLockedCues.length > 0 ? ` · ${staleLockedCues.length} blocked by stale device locks` : '' }}. Dependencies must be included and sequence numbers must be unique.</p></div>
    <el-select v-model="selectedCueIds" multiple collapse-tags collapse-tags-tooltip placeholder="Choose locked cues">
      <el-option v-for="cue in runnableLockedCues" :key="cue.id" :value="cue.id" :label="`${cue.cue_code} · v${cue.version} · ${cue.name}`" />
    </el-select>
    <el-button v-if="canProgram" type="primary" :icon="Play" :loading="runs.running" :disabled="selectedCueIds.length === 0" @click="run">Run offline rehearsal</el-button>
  </section>
  <section v-if="staleRunFailure.length > 0" class="data-section stale-run-detail">
    <div class="section-heading"><div><p class="eyebrow">REHEARSAL BLOCKED · DEVICE VERSION LOCK</p><h2>Affected cues and devices</h2></div></div>
    <div v-for="cue in staleRunFailure" :key="cue.cue_id" class="stale-run-cue">
      <strong>{{ cue.cue_code }} v{{ cue.cue_version }}</strong>
      <span class="subtle">{{ cue.reason }}</span>
      <el-table :data="cue.stale_devices" size="small">
        <el-table-column label="Device" prop="device_code" min-width="140" />
        <el-table-column label="Old (approved) version" width="150"><template #default="scope">v{{ scope.row.locked_version }}</template></el-table-column>
        <el-table-column label="New (live) version" width="140"><template #default="scope"><span class="version-mismatch">v{{ scope.row.current_version }}</span></template></el-table-column>
        <el-table-column label="Why it is blocked" min-width="200"><template #default="scope">{{ scope.row.reason }}</template></el-table-column>
      </el-table>
    </div>
  </section>
  <div class="run-selector">
    <button v-for="item in runs.items" :key="item.id" :class="{ selected: runs.selectedId === item.id }" @click="runs.selectedId = item.id">
      <span>#{{ item.id }}</span><strong>{{ item.cue_set_version }}</strong><el-tag :type="statusType(item.run_status)" effect="plain">{{ item.run_status.replaceAll('_',' ') }}</el-tag><small>{{ formatTimestamp(item.finished_at) }}</small>
    </button>
  </div>
  <template v-if="selected">
    <section class="run-summary" :class="selected.highest_severity">
      <div><p class="eyebrow">RUN #{{ selected.id }} · VERSION {{ selected.version }}</p><h2>{{ selected.run_status.replaceAll('_', ' ') }}</h2></div>
      <div class="run-stat"><span>Highest evidence</span><strong>{{ selected.highest_severity }}</strong></div>
      <div class="run-stat"><span>Rule results</span><strong>{{ selected.rule_results.length }}</strong></div>
      <div class="run-stat"><span>Collision windows</span><strong>{{ selected.collision_windows.length }}</strong></div>
      <span v-if="polling" class="polling"><RefreshCw :size="14" /> refreshing</span>
    </section>
    <section class="data-section timeline-section">
      <div class="section-heading"><div><p class="eyebrow">TIMELINE SNAPSHOT</p><h2>{{ selected.cue_set_version }}</h2></div><span>{{ selected.timeline_snapshot.timeline_step_ms }} ms evaluation step</span></div>
      <TimelineTrack :events="selected.timeline_snapshot.timeline" />
    </section>
    <div class="evidence-layout">
      <section class="data-section">
        <div class="section-heading"><div><p class="eyebrow">RULE OUTPUT</p><h2>Interlock evidence</h2></div><span>{{ selected.rule_results.length }} checks</span></div>
        <RuleEvidenceTable :evidence="selected.rule_results" />
      </section>
      <aside class="assumption-panel">
        <p class="eyebrow">SNAPSHOT ASSUMPTIONS</p>
        <ul><li v-for="assumption in selected.timeline_snapshot.assumptions" :key="assumption">{{ assumption }}</li></ul>
        <p class="eyebrow">COLLISION WINDOWS</p>
        <div v-if="selected.collision_windows.length === 0" class="empty-inline">No shared-zone overlap in this snapshot.</div>
        <div v-for="window in selected.collision_windows" :key="`${window.safety_zone}-${window.start_ms}`" class="collision-row"><strong>{{ window.safety_zone }}</strong><span>{{ window.cue_codes.join(' + ') }}</span><small>{{ window.start_ms }}–{{ window.end_ms }} ms</small></div>
      </aside>
    </div>
    <section class="review-strip">
      <el-input v-model="reason" type="textarea" :rows="2" maxlength="500" show-word-limit />
      <div>
        <el-button v-if="canProgram && selected.run_status === 'evaluated'" type="primary" :icon="Send" @click="submit">Submit safety review</el-button>
        <el-button v-if="canReview && selected.run_status === 'pending_review'" type="success" :icon="CheckCircle2" @click="review('approve')">Approve rehearsal evidence</el-button>
        <el-button v-if="canReview && selected.run_status === 'pending_review'" :icon="XCircle" @click="review('reject')">Reject evidence</el-button>
      </div>
      <p>Human approval records review of this offline snapshot only. It is not an executable cue, machinery release, or live safety authorization.</p>
    </section>
  </template>
  <div v-else class="empty-state"><Play :size="28" /><h2>No rehearsal selected</h2><p>Choose locked cue versions and run the deterministic evaluator.</p></div>
</template>

<style scoped>
.stale-locked-alert {
  margin: 12px 0;
}
.stale-cue-line {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  margin: 4px 0;
  font-size: 13px;
}
.stale-device-chip {
  background: var(--el-color-warning-light-8);
  border-radius: 4px;
  padding: 1px 6px;
  font-size: 12px;
}
.stale-run-detail {
  border-color: var(--el-color-danger-light-5);
}
.stale-run-cue {
  margin-bottom: 12px;
}
.version-mismatch {
  color: var(--el-color-danger);
  font-weight: 700;
}
</style>
