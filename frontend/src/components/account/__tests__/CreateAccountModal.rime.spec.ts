import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { BUILTIN_PLATFORM_CATALOG } from '@/constants/platformCatalog'

const createSource = readFileSync(resolve(process.cwd(), 'src/components/account/CreateAccountModal.vue'), 'utf8')
const editSource = readFileSync(resolve(process.cwd(), 'src/components/account/EditAccountModal.vue'), 'utf8')
const groupSource = readFileSync(resolve(process.cwd(), 'src/views/admin/GroupsView.vue'), 'utf8')

describe('Rime account setup', () => {
  it('registers Rime and routes its selector to the API key form', () => {
    expect(BUILTIN_PLATFORM_CATALOG.platforms.find(spec => spec.id === 'rime')?.display_name).toBe('Rime')
    expect(createSource).toContain("spec.id === 'rime'")
    expect(createSource).toContain('@click="selectExtraAPIKeyPlatform(spec.id)"')
    expect(createSource).toContain("if (platform === 'rime')")
    expect(createSource).toContain("allowedModels.value = ['coda', 'mistv3']")
    expect(createSource).toContain("case 'rime':\n      return 'Rime API Key'")
  })

  it('uses the official endpoint when creating and clearing an edited URL', () => {
    expect(createSource).toContain("apiKeyBaseUrl.value = 'https://coda.api.rime.ai'")
    expect(editSource).toContain("if (props.account?.platform === 'rime') return 'https://coda.api.rime.ai'")
  })

  it('exposes TTS group pricing on create and edit', () => {
    for (const form of ['createForm', 'editForm']) {
      expect(groupSource).toContain(`${form}.platform === 'grok' || ${form}.platform === 'rime'`)
      expect(groupSource).toContain(`v-model.number="${form}.audio_tts_price_per_million_chars"`)
      expect(groupSource).toContain(`v-if="${form}.platform !== 'rime'"`)
    }
    expect(groupSource).toContain('if (platform === "rime") entry.billing_mode = "per_request"')
  })
})
