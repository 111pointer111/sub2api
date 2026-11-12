import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import OpenAIPlatformUsageCell from '../OpenAIPlatformUsageCell.vue'
import AccountUsageCell from '../AccountUsageCell.vue'
import { hasOpenAIPlatformUsage, isOfficialOpenAIPlatformUrl } from '@/utils/openaiPlatformUsage'
import type { Account, OpenAIPlatformUsage } from '@/types'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

const { getOpenAIPlatformUsage } = vi.hoisted(() => ({ getOpenAIPlatformUsage: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getOpenAIPlatformUsage } } }))

const account = {
  id: 42, name: 'OpenAI', platform: 'openai', type: 'apikey',
  credentials: { base_url: 'https://api.openai.com', openai_usage_project_id: 'proj_example' },
  credentials_status: { has_api_key: true, has_openai_admin_api_key: true },
  updated_at: '2026-11-12T10:00:00Z'
} as Account

const report: OpenAIPlatformUsage = {
  project_id: 'proj_example', period_start: '2026-11-01T00:00:00Z', period_end: '2026-11-12T10:00:00Z',
  fetched_at: '2026-11-12T10:00:00Z', monthly_cost: '12.34', today_cost: '0.56', currency: 'USD',
  requests: 123, input_tokens: 1000, output_tokens: 500, cached_input_tokens: 400
}

function options() {
  return { props: { account }, global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { en, zh } })] } }
}

describe('OpenAI Platform official usage', () => {
  beforeEach(() => getOpenAIPlatformUsage.mockReset().mockResolvedValue(report))
  afterEach(() => vi.useRealTimers())

  it('shows official costs, explicit scope and generation usage through the account cell', async () => {
    const wrapper = mount(AccountUsageCell, options())
    await flushPromises()
    expect(getOpenAIPlatformUsage).toHaveBeenCalledWith(42, false)
    expect(wrapper.text()).toContain('OpenAI 官方账单')
    expect(wrapper.text()).toContain('项目 proj_example · UTC')
    expect(wrapper.text()).toContain('$12.34')
    expect(wrapper.text()).toContain('$0.56')
    expect(wrapper.text()).toContain('生成请求 123 次')
    expect(wrapper.text()).toContain('输入中缓存命中 400 Token')
    wrapper.unmount()
  })

  it('keeps the successful amounts and timestamp when refresh fails and supports retry', async () => {
    const wrapper = mount(OpenAIPlatformUsageCell, options())
    await flushPromises()
    getOpenAIPlatformUsage.mockRejectedValueOnce({ code: 502, reason: 'OPENAI_PLATFORM_USAGE_AUTH_FAILED' })
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('Admin API Key')
    expect(wrapper.text()).toContain('$12.34')
    expect(wrapper.text()).toContain('更新于')
    getOpenAIPlatformUsage.mockResolvedValueOnce({ ...report, monthly_cost: '15' })
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('$15')
    wrapper.unmount()
  })

  it('refreshes every five minutes while visible and stops after unmount', async () => {
    vi.useFakeTimers()
    const wrapper = mount(OpenAIPlatformUsageCell, options())
    await flushPromises()
    expect(getOpenAIPlatformUsage).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(5 * 60 * 1000)
    expect(getOpenAIPlatformUsage).toHaveBeenCalledTimes(2)
    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(5 * 60 * 1000)
    expect(getOpenAIPlatformUsage).toHaveBeenCalledTimes(2)
  })

  it('ignores an old in-flight response after switching accounts', async () => {
    let resolveOld: (value: OpenAIPlatformUsage) => void = () => {}
    getOpenAIPlatformUsage.mockImplementationOnce(() => new Promise<OpenAIPlatformUsage>((resolve) => { resolveOld = resolve }))
    const wrapper = mount(OpenAIPlatformUsageCell, options())
    await wrapper.setProps({ account: { ...account, id: 43 } })
    await flushPromises()
    resolveOld({ ...report, monthly_cost: '999' })
    await flushPromises()
    expect(wrapper.text()).toContain('$12.34')
    expect(wrapper.text()).not.toContain('$999')
    wrapper.unmount()
  })

  it('keeps the report when account-list polling supplies the same account values', async () => {
    const wrapper = mount(OpenAIPlatformUsageCell, options())
    await flushPromises()
    await wrapper.setProps({ account: { ...account, credentials: { ...account.credentials } } })
    await flushPromises()
    expect(getOpenAIPlatformUsage).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).toContain('$12.34')
    wrapper.unmount()
  })

  it('only queries configured official API-key accounts', async () => {
    expect(isOfficialOpenAIPlatformUrl('https://api.openai.com/v1/')).toBe(true)
    expect(isOfficialOpenAIPlatformUrl('https://api.openai.com:443/v1')).toBe(true)
    for (const base_url of ['http://api.openai.com', 'https://api.openai.com.evil.test', 'https://user@api.openai.com', 'https://api.openai.com/other']) {
      expect(hasOpenAIPlatformUsage({ ...account, credentials: { base_url } })).toBe(false)
    }
    const wrapper = mount(AccountUsageCell, { ...options(), props: { account: { ...account, credentials_status: { has_api_key: true } } } })
    await flushPromises()
    expect(getOpenAIPlatformUsage).not.toHaveBeenCalled()
    expect(wrapper.findComponent(OpenAIPlatformUsageCell).exists()).toBe(false)
    wrapper.unmount()
  })
})
