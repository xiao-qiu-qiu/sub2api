import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { TrashEntry, TrashKind } from '@/api/admin/trash'
import RecycleBinDialog from '../RecycleBinDialog.vue'

const { listTrash } = vi.hoisted(() => ({ listTrash: vi.fn() }))
vi.mock('@/api/admin/trash', () => ({
  listTrash,
  restoreTrash: vi.fn(),
  permanentlyDeleteTrash: vi.fn()
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() })
}))
vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key })
}))

const wrappers: ReturnType<typeof mount>[] = []
const entry: TrashEntry = {
  id: 7,
  name: 'Archived account',
  deleted_at: '2026-11-03T00:00:00Z',
  platform: 'openai',
  type: 'oauth'
}

async function render(entries: TrashEntry[], kind: TrashKind = 'accounts') {
  listTrash.mockResolvedValue({ items: entries, total: entries.length, page: 1, page_size: 20 })
  const wrapper = mount(RecycleBinDialog, {
    props: { show: true, kind },
    global: {
      stubs: {
        BaseDialog: {
          props: ['show'],
          template: '<div v-if="show"><slot /><slot name="footer" /></div>'
        },
        Icon: true,
        Pagination: true
      }
    }
  })
  wrappers.push(wrapper)
  await flushPromises()
  return wrapper
}

beforeEach(() => vi.clearAllMocks())
afterEach(() => wrappers.splice(0).forEach(wrapper => wrapper.unmount()))

describe('account recycle bin deletion notes', () => {
  it('shows the entire multiline note as text without truncation or HTML rendering', async () => {
    const note = `首行备注\n${'长备注'.repeat(300)}\n<script>unsafe()</script>`
    const wrapper = await render([{ ...entry, delete_note: note }])

    expect(wrapper.findAll('th').map(header => header.text())).toContain('trash.deleteNote')
    const noteCell = wrapper.get('tbody tr').findAll('td')[3]
    expect(noteCell.element.textContent).toBe(note)
    expect(noteCell.find('script').exists()).toBe(false)
    expect(noteCell.get('div').classes()).toContain('whitespace-pre-wrap')
    expect(noteCell.get('div').classes()).toContain('[overflow-wrap:anywhere]')
  })

  it.each([undefined, '', ' \t\n '])('shows a dash for an empty note: %j', async note => {
    const wrapper = await render([{ ...entry, delete_note: note }])

    expect(wrapper.get('tbody tr').findAll('td')[3].text()).toBe('—')
  })

  it('does not add the note column to the proxy recycle bin', async () => {
    const wrapper = await render([{
      ...entry,
      protocol: 'http',
      host: 'proxy.example',
      port: 8080,
      delete_note: 'Account-only note'
    }], 'proxies')

    expect(wrapper.findAll('th').map(header => header.text())).not.toContain('trash.deleteNote')
    expect(wrapper.text()).not.toContain('Account-only note')
    expect(wrapper.get('tbody tr').findAll('td')).toHaveLength(4)
  })
})
