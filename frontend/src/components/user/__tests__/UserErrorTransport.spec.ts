import { shallowMount } from '@vue/test-utils'
import { expect, it, vi } from 'vitest'
import UserErrorRequestsTable from '../UserErrorRequestsTable.vue'

vi.mock('vue-i18n', async (original) => ({ ...(await original<typeof import('vue-i18n')>()), useI18n: () => ({ t: (s: string) => s }) }))

it('shows only the final public message and offers no error detail action', () => {
  const message = 'The gateway returned this message.'
  const wrapper = shallowMount(UserErrorRequestsTable, {
    props: { rows: [], loading: false, page: 1, pageSize: 20, total: 0 },
    global: { stubs: { DataTable: {
      name: 'DataTable', props: { clickableRows: Boolean },
      data: () => ({ row: { category: 'transport_error', message } }),
      template: '<div><slot name="cell-message" :row="row"/></div>',
    } } },
  })
  expect(wrapper.text()).toContain(message)
  expect(wrapper.find('[title]').attributes('title')).toBe(message)
  expect(wrapper.findComponent({ name: 'DataTable' }).props('clickableRows')).toBeFalsy()
  expect(wrapper.find('[role=dialog]').exists()).toBe(false)
})
