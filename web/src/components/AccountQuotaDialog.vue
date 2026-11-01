<script setup lang="ts">
import { onBeforeUnmount, onDeactivated, ref, watch } from 'vue'
import { api } from '@/api'
import { useI18n } from '@/i18n'
import type { Account, AccountQuota } from '@/types'
import DetailDialog from './DetailDialog.vue'
import UiSelect from './UiSelect.vue'

const props = defineProps<{ account: Account }>()
const emit = defineEmits<{ close: [] }>()
const { t, errorText } = useI18n()
const model = ref('')
const quota = ref<AccountQuota | null>(null)
const loading = ref(false)
const error = ref('')
let controller: AbortController | undefined

// load 不复用其他模型的旧快照，切换模型或关闭弹窗时取消请求。
async function load(): Promise<void> {
  controller?.abort()
  quota.value = null
  error.value = ''
  loading.value = false
  if (!model.value) return
  const current = new AbortController()
  controller = current
  loading.value = true
  try {
    const result = await api.accountQuota(props.account.id, model.value, current.signal)
    if (!current.signal.aborted) quota.value = result
  } catch (reason) {
    if (!current.signal.aborted) error.value = errorText(reason)
  } finally {
    if (controller === current) loading.value = false
  }
}

watch(
  () => props.account.models,
  (models) => {
    if (!models.includes(model.value)) model.value = models[0] ?? ''
  },
  { immediate: true },
)
watch(model, () => void load(), { immediate: true })
onBeforeUnmount(() => controller?.abort())
onDeactivated(() => {
  controller?.abort()
  emit('close')
})

function number(value: number | null): string {
  return value === null ? t('quota.unknown') : value.toLocaleString()
}

function dateTime(value: string): string {
  return new Date(value).toLocaleString()
}
</script>

<template>
  <DetailDialog
    :open="true"
    :title="`${account.label} · ${t('accounts.quota')}`"
    :description="t('quota.description')"
    @close="emit('close')"
  >
    <div class="space-y-4">
      <p class="text-xs leading-5 text-gray-400">{{ t('quota.hint') }}</p>
      <p v-if="account.models.length === 0" class="text-gray-400">
        {{ t('quota.emptyModels') }}
      </p>
      <div v-else class="flex flex-wrap items-end gap-3">
        <label class="min-w-0 flex-1 space-y-1 text-xs text-gray-400">
          <span>{{ t('quota.model') }}</span>
          <UiSelect v-model="model" class="w-full">
            <option v-for="id in account.models" :key="id" :value="id">{{ id }}</option>
          </UiSelect>
        </label>
        <button
          type="button"
          class="rounded border border-line bg-raised px-3 py-2 text-xs text-gray-300 hover:bg-line disabled:opacity-50"
          :disabled="loading || !model"
          :aria-busy="loading"
          @click="load"
        >
          {{ loading ? t('common.loading') : t('accounts.refresh') }}
        </button>
      </div>
      <p v-if="error" role="alert" class="text-sm text-red-300">{{ error }}</p>
      <p v-else-if="loading" role="status" class="text-sm text-gray-400">
        {{ t('common.loading') }}
      </p>
      <template v-else-if="quota">
        <p v-if="quota.metrics.length === 0" class="rounded border border-line p-3 text-gray-400">
          {{ t(quota.status === 'unavailable' ? 'quota.unavailable' : 'quota.noMetrics') }}
        </p>
        <div v-else class="overflow-x-auto rounded border border-line">
          <table class="w-full text-left text-sm">
            <thead class="border-b border-line bg-raised text-xs text-gray-400">
              <tr>
                <th scope="col" class="px-3 py-2">{{ t('quota.metric') }}</th>
                <th scope="col" class="px-3 py-2 text-right">{{ t('quota.remaining') }}</th>
                <th scope="col" class="px-3 py-2 text-right">{{ t('quota.limit') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="(metric, index) in quota.metrics"
                :key="index"
                class="border-b border-line/40"
              >
                <td class="max-w-72 px-3 py-2 [overflow-wrap:anywhere]">
                  {{
                    metric.name === 'successful_video_generations' ? t('quota.video') : metric.name
                  }}
                </td>
                <td
                  class="px-3 py-2 text-right tabular-nums"
                  :class="{ 'text-red-300': metric.remaining === 0 }"
                >
                  {{ number(metric.remaining) }}
                </td>
                <td class="px-3 py-2 text-right text-gray-400 tabular-nums">
                  {{ number(metric.limit) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <section class="space-y-2 rounded border border-line p-3">
          <h3 class="text-sm font-medium">{{ t('quota.recovery') }}</h3>
          <p v-if="quota.recoveries.length === 0" class="text-xs text-gray-400">
            {{ t('quota.recoveryUnknown') }}
          </p>
          <p v-for="recovery in quota.recoveries" :key="recovery.scope" class="text-xs leading-5">
            {{ t(recovery.scope === '*' ? 'quota.scopeGlobal' : 'quota.scopeModel') }} ·
            {{ dateTime(recovery.until) }}
            <span class="block text-gray-400">
              {{
                t(
                  recovery.source === 'upstream'
                    ? 'quota.fromUpstream'
                    : recovery.source === 'estimated'
                      ? 'quota.fromEstimate'
                      : 'quota.fromUnknown',
                )
              }}
            </span>
          </p>
        </section>
        <p class="text-xs text-gray-500">
          {{ t('quota.snapshot').replace('{time}', dateTime(quota.checked_at)) }}
        </p>
      </template>
    </div>
  </DetailDialog>
</template>
