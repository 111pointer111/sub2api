import type { Account } from '@/types'

export function isOfficialOpenAIPlatformUrl(value: unknown): boolean {
  if (value == null || value === '') return true
  if (typeof value !== 'string') return false
  if (!value.trim()) return true
  try {
    const url = new URL(value.trim())
    return url.protocol === 'https:' && url.hostname === 'api.openai.com' &&
      (!url.port || url.port === '443') && !url.username && !url.password &&
      !url.search && !url.hash && ['', '/v1'].includes(url.pathname.replace(/\/+$/, ''))
  } catch {
    return false
  }
}

export function hasOpenAIPlatformUsage(account: Account): boolean {
  return account.platform === 'openai' && account.type === 'apikey' &&
    isOfficialOpenAIPlatformUrl(account.credentials?.base_url) &&
    account.credentials_status?.has_openai_admin_api_key === true
}
