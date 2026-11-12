<template>
  <div class="min-w-0 space-y-1 text-xs text-gray-700 dark:text-gray-200" data-testid="openai-platform-usage">
    <div class="font-medium">{{ t('admin.accounts.openaiPlatformUsage.title') }}</div>
    <p class="break-all text-gray-600 dark:text-gray-300">
      {{ projectID ? t('admin.accounts.openaiPlatformUsage.projectScope', { id: projectID }) : t('admin.accounts.openaiPlatformUsage.organizationScope') }}
    </p>
    <template v-if="usage">
      <div class="flex flex-wrap gap-x-3 gap-y-1 tabular-nums">
        <span :title="`USD ${usage.monthly_cost}`">{{ t('admin.accounts.openaiPlatformUsage.monthlyCost') }} <strong>${{ formatCost(usage.monthly_cost) }}</strong></span>
        <span :title="`USD ${usage.today_cost}`">{{ t('admin.accounts.openaiPlatformUsage.todayCost') }} ${{ formatCost(usage.today_cost) }}</span>
      </div>
      <details>
        <summary class="cursor-pointer rounded focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500">
          {{ t('admin.accounts.openaiPlatformUsage.monthlyUsage') }}
        </summary>
        <div class="mt-1 space-y-0.5 tabular-nums">
          <div>{{ t('admin.accounts.openaiPlatformUsage.requests', { count: usage.requests.toLocaleString() }) }}</div>
          <div>{{ t('admin.accounts.openaiPlatformUsage.inputTokens', { count: usage.input_tokens.toLocaleString() }) }}</div>
          <div>{{ t('admin.accounts.openaiPlatformUsage.outputTokens', { count: usage.output_tokens.toLocaleString() }) }}</div>
          <div>{{ t('admin.accounts.openaiPlatformUsage.cachedTokens', { count: usage.cached_input_tokens.toLocaleString() }) }}</div>
        </div>
      </details>
      <p class="text-gray-600 dark:text-gray-300">{{ t('admin.accounts.openaiPlatformUsage.updatedAt', { time: formattedTime }) }}</p>
    </template>
    <p v-else-if="loading" role="status">{{ t('common.loading') }}</p>
    <p v-if="error" role="alert" class="text-red-700 dark:text-red-300">{{ error }}</p>
    <p v-if="error && usage" class="text-gray-600 dark:text-gray-300">{{ t('admin.accounts.openaiPlatformUsage.stale') }}</p>
    <button
      type="button"
      class="rounded text-primary-700 hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 disabled:cursor-wait disabled:opacity-50 dark:text-primary-300"
      :disabled="loading"
      @click="refresh(true)"
    >
      {{ t('common.refresh') }}
    </button>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import { extractApiErrorCode } from '@/utils/apiError'
import type { Account, OpenAIPlatformUsage } from '@/types'

const props = defineProps<{ account: Account; manualRefreshToken?: number }>()
const { t } = useI18n()
const usage = ref<OpenAIPlatformUsage | null>(null)
const loading = ref(false)
const error = ref('')
let generation = 0
let disposed = false
const formattedTime = computed(() => usage.value ? new Date(usage.value.fetched_at).toLocaleString() : '')
const projectID = computed(() => usage.value
  ? usage.value.project_id
  : String(props.account.credentials?.openai_usage_project_id ?? '').trim())
const formatCost = (value: string) => Number(value).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 6 })

async function refresh(force = false) {
  if (loading.value || disposed) return
  const currentGeneration = generation
  loading.value = true
  error.value = ''
  try {
    const result = await adminAPI.accounts.getOpenAIPlatformUsage(props.account.id, force)
    if (!disposed && generation === currentGeneration) usage.value = result
  } catch (cause: unknown) {
    if (disposed || generation !== currentGeneration) return
    const code = extractApiErrorCode(cause)
    error.value = code === 'OPENAI_PLATFORM_USAGE_AUTH_FAILED'
      ? t('admin.accounts.openaiPlatformUsage.authFailed')
      : code === 'OPENAI_PLATFORM_USAGE_RATE_LIMITED'
        ? t('admin.accounts.openaiPlatformUsage.rateLimited')
        : t('admin.accounts.openaiPlatformUsage.failed')
  } finally {
    if (!disposed && generation === currentGeneration) loading.value = false
  }
}

watch([
  () => props.account.id,
  () => props.account.updated_at,
  () => props.account.credentials?.openai_usage_project_id
], () => {
  generation++
  usage.value = null
  loading.value = false
  void refresh()
}, { immediate: true })
watch(() => props.manualRefreshToken, () => { void refresh(true) })
const timer = setInterval(() => {
  if (document.visibilityState === 'visible') void refresh()
}, 5 * 60 * 1000)
onBeforeUnmount(() => {
  disposed = true
  clearInterval(timer)
})
</script>
