import { mount, flushPromises } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { expect, it, vi } from 'vitest'
import SecondaryEgressSettings from './SecondaryEgressSettings.vue'
import zh from '@/i18n/locales/zh'
const api = vi.hoisted(() => ({ listSecondaryEgress: vi.fn(), updateSecondaryEgress: vi.fn() }))
const notices = vi.hoisted(() => ({ showSuccess: vi.fn(), showError: vi.fn() }))
vi.mock('@/api/admin/settings', () => api)
vi.mock('@/stores', () => ({ useAppStore: () => notices }))
it('edits both routes for only the selected paired site', async () => {
  api.listSecondaryEgress.mockResolvedValue(['office', 'other'].map(node => ({ node, processing_proxy_url: '', forward_proxy_url: '' })))
  api.updateSecondaryEgress.mockResolvedValue({})
  const w = mount(SecondaryEgressSettings, { global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh } })] } })
  await flushPromises()
  const [local, forward] = w.findAll('input')
  await local.setValue('http://proxy:9101')
  await forward.setValue('http://proxy:9102')
  await w.get('button').trigger('click'); await flushPromises()
  expect(api.updateSecondaryEgress).toHaveBeenCalledWith('office', { processing_proxy_url: 'http://proxy:9101', forward_proxy_url: 'http://proxy:9102' })
  expect(notices.showSuccess).toHaveBeenCalledOnce()
  w.unmount()
})
