<script setup>
import { onMounted, onUnmounted, ref, computed } from 'vue'

const state = ref({ sets: [], events: [], leader: '', self: '' })
const err = ref('')
const modal = ref(null)
const modalSet = ref(null)
const modalTarget = ref('')
const busy = ref(false)
let timer = null

async function refresh() {
  try {
    const r = await fetch('/api/state')
    if (!r.ok) throw new Error(`HTTP ${r.status}`)
    state.value = await r.json()
    err.value = ''
  } catch (e) {
    err.value = String(e)
  }
}

function openSwitch(set) {
  modalSet.value = set
  const alt = set.health?.find((h) => h.addr !== set.primary)
  modalTarget.value = alt ? alt.addr : ''
  modal.value.showModal()
}

async function doSwitch() {
  busy.value = true
  try {
    const r = await fetch(`/api/sets/${modalSet.value.name}/switch`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ to: modalTarget.value, reason: 'admin-ui' }),
    })
    err.value = r.ok ? '' : `switch failed: HTTP ${r.status}`
    modal.value.close()
  } catch (e) {
    err.value = String(e)
  } finally {
    busy.value = false
  }
}

function badgeClass(h) {
  if (!h.ok) return 'badge-error'
  if (h.role === 'primary') return 'badge-success'
  return 'badge-info'
}

const eventsDesc = computed(() => [...(state.value.events || [])].reverse().slice(0, 15))

onMounted(() => {
  refresh()
  timer = setInterval(refresh, 2000)
})
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <div class="min-h-screen bg-base-200 p-6">
    <div class="mx-auto max-w-5xl space-y-6">
      <div class="flex items-center justify-between">
        <h1 class="text-2xl font-bold">DBAdmin <span class="text-base-content/50 text-sm">bank_dcn · MySQL sets</span></h1>
        <div class="flex items-center gap-2">
          <span class="badge" :class="state.leader ? 'badge-primary' : 'badge-warning'">
            {{ state.leader ? `leader ${state.leader}` : 'no leader' }}
          </span>
          <span class="badge badge-ghost">self {{ state.self }}</span>
        </div>
      </div>

      <div v-if="err" class="alert alert-error text-sm py-2">{{ err }}</div>

      <div v-for="set in state.sets" :key="set.name" class="card bg-base-100 shadow">
        <div class="card-body p-5">
          <div class="flex items-center justify-between">
            <h2 class="card-title">
              {{ set.name }}
              <span class="badge badge-outline">proxy :{{ set.port }}</span>
            </h2>
            <button class="btn btn-primary btn-sm" @click="openSwitch(set)">Switch primary</button>
          </div>
          <div class="overflow-x-auto">
            <table class="table table-sm">
              <thead>
                <tr><th>node</th><th>role</th><th>health</th><th>lag</th></tr>
              </thead>
              <tbody>
                <tr v-for="h in set.health" :key="h.addr" :class="{ 'bg-primary/10': h.addr === set.primary }">
                  <td class="font-mono text-xs">{{ h.addr }}</td>
                  <td>
                    <span class="badge badge-sm" :class="badgeClass(h)">{{ h.role }}</span>
                    <span v-if="h.addr === set.primary" class="ml-2 text-xs opacity-60">desired</span>
                  </td>
                  <td>{{ h.ok ? '●' : '○' }}</td>
                  <td>{{ h.lagSeconds ?? '—' }}s</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>

      <div class="card bg-base-100 shadow">
        <div class="card-body p-5">
          <h2 class="card-title text-lg">Events</h2>
          <ul class="timeline timeline-vertical timeline-compact">
            <li v-for="(e, i) in eventsDesc" :key="i">
              <div class="timeline-start text-xs opacity-70">{{ (e.time || '').replace('T', ' ').slice(0, 19) }}</div>
              <div class="timeline-middle">·</div>
              <div class="timeline-end timeline-box text-sm">
                <span class="font-mono text-xs">{{ e.type }}</span>
                <span v-if="e.set" class="ml-2">{{ e.set }}</span>
                <span v-if="e.from" class="ml-2 opacity-70 font-mono text-xs">{{ e.from }} → {{ e.to }}</span>
                <span v-if="e.reason" class="ml-2 opacity-70">{{ e.reason }}</span>
              </div>
            </li>
          </ul>
        </div>
      </div>
    </div>

    <dialog ref="modal" class="modal">
      <div class="modal-box">
        <h3 class="font-bold text-lg">Switch primary — {{ modalSet?.name }}</h3>
        <p class="py-2 text-sm opacity-80">
          DBKeeper will: wait for the target to catch up → fence the old primary
          (super_read_only) → promote the target → re-attach the old primary as
          replica → repoint DBProxy routing.
        </p>
        <select v-model="modalTarget" class="select select-bordered w-full">
          <option v-for="h in modalSet?.health || []" :key="h.addr" :value="h.addr" :disabled="h.addr === modalSet?.primary">
            {{ h.addr }} ({{ h.role }}{{ h.lagSeconds != null ? `, lag ${h.lagSeconds}s` : '' }})
          </option>
        </select>
        <div class="modal-action">
          <form method="dialog"><button class="btn btn-ghost">Cancel</button></form>
          <button class="btn btn-primary" :disabled="!modalTarget || busy" @click="doSwitch">
            <span v-if="busy" class="loading loading-spinner loading-xs"></span>
            Confirm switch
          </button>
        </div>
      </div>
      <form method="dialog" class="modal-backdrop"><button>close</button></form>
    </dialog>
  </div>
</template>
