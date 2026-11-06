import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import type { AccountListItem } from '@/types'
import { qualityAccountStatusOptions } from '@/utils/qualityAccountFilter'
import QualityAccountSelector from '../QualityAccountSelector.vue'

vi.mock('vue-i18n', async () => {
  const { default: qualityOps } = await import('@/i18n/locales/zh/qualityOps')
  return {
    useI18n: () => ({
      t: (key: string, params: Record<string, unknown> = {}) => {
        const message = (qualityOps as Record<string, unknown>)[key.replace(/^qualityOps\./, '')]
        return typeof message === 'string'
          ? message.replace(/\{(\w+)\}/g, (_, name: string) => String(params[name] ?? ''))
          : key
      },
    }),
  }
})

enableAutoUnmount(afterEach)

type SelectorProps = InstanceType<typeof QualityAccountSelector>['$props']
type SelectorOverrides = Partial<SelectorProps>

const makeAccounts = (): AccountListItem[] => [
  { id: 101, name: 'Fixture Alpha', platform: 'openai', type: 'oauth', status: 'active' },
  { id: 202, name: 'Fixture Beta', platform: 'openai', type: 'oauth', status: 'active' },
  { id: 303, name: 'Fixture Gamma', platform: 'openai', type: 'apikey', status: 'active' },
] as AccountListItem[]

function mountSelector(overrides: SelectorOverrides = {}) {
  return mount(QualityAccountSelector, {
    props: {
      modelValue: [],
      search: '',
      group: '',
      type: '',
      statuses: ['active'],
      scope: 'all',
      accounts: makeAccounts(),
      groups: [],
      accountsLoading: false,
      selectingAccounts: false,
      accountsError: '',
      accountPage: 1,
      accountPages: 1,
      accountsTotal: 3,
      bulk: false,
      disabledReason: () => '',
      ...overrides,
    },
  })
}

type SelectorWrapper = ReturnType<typeof mountSelector>

function accountRow(wrapper: SelectorWrapper, name: string) {
  const row = wrapper.findAll('[aria-busy] > *').find(item => item.text().includes(name))
  if (!row) throw new Error(`Missing fixture account row: ${name}`)
  return row
}

function accountCheckboxes(wrapper: SelectorWrapper) {
  return wrapper.findAll<HTMLInputElement>('[aria-busy] input[type="checkbox"]')
}

function expectNoReinclude(wrapper: SelectorWrapper) {
  expect(wrapper.emitted('toggleReinclude')).toBeUndefined()
  expect(wrapper.emitted('update:modelValue')).toBeUndefined()
}

describe('QualityAccountSelector preview', () => {
  it('renders account previews without account checkboxes or selection actions', async () => {
    const wrapper = mountSelector()

    expect(accountCheckboxes(wrapper)).toHaveLength(0)
    expect(wrapper.findAll('input[type="checkbox"]')).toHaveLength(qualityAccountStatusOptions.length)
    expect(wrapper.find('[data-testid="quality-select-page"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="quality-select-all"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="quality-clear-selection"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="quality-account-count"]').text()).toContain('3')

    for (const account of makeAccounts()) {
      const row = accountRow(wrapper, account.name)
      expect(row.text()).toContain(`#${account.id}`)
      expect(row.find('button').exists()).toBe(false)
      await row.trigger('click')
    }
    expectNoReinclude(wrapper)
  })

  it.each(['all', 'manual'] as const)('offers no reinclude actions outside template editing in %s scope', async scope => {
    const wrapper = mountSelector({
      scope,
      excludedAccountIds: [101],
      reincludeAccountIds: [101],
    })
    const row = accountRow(wrapper, 'Fixture Alpha')

    expect(row.text()).not.toContain('已排除')
    expect(row.text()).not.toContain('待重新纳入')
    expect(row.find('button').exists()).toBe(false)
    await row.trigger('click')
    expectNoReinclude(wrapper)
  })

  it('keeps status filtering and searching usable without reincluding an account', async () => {
    const wrapper = mountSelector()

    await wrapper.get('[data-testid="quality-account-status-error"]').setValue(true)
    expect(wrapper.emitted('update:statuses')).toEqual([[['active', 'error']]])
    await wrapper.get('#quality-account-search').setValue('Fixture')
    await wrapper.get('#quality-account-search').trigger('keydown.enter')

    expect(wrapper.emitted('update:search')).toEqual([['Fixture']])
    expect(wrapper.emitted('search')).toEqual([[1], [1]])
    expectNoReinclude(wrapper)
  })
})

describe('QualityAccountSelector explicit template reinclusion', () => {
  it('offers reinclusion only for excluded accounts while keeping the scope locked', async () => {
    const excludedAccountIds = [101]
    const reincludeAccountIds: number[] = []
    const wrapper = mountSelector({ scopeLocked: true, excludedAccountIds, reincludeAccountIds })
    const row = accountRow(wrapper, 'Fixture Alpha')

    expect(accountCheckboxes(wrapper)).toHaveLength(0)
    expect(wrapper.find('[data-testid="quality-account-scope-all"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="quality-account-scope-manual"]').exists()).toBe(false)
    expect(row.text()).toContain('已排除')
    expect(row.text()).not.toContain('待重新纳入')
    const button = row.get<HTMLButtonElement>('button')
    expect(button.text()).toBe('重新纳入')
    expect(button.attributes('type')).toBe('button')
    expect(button.element.disabled).toBe(false)
    expect(accountRow(wrapper, 'Fixture Beta').find('button').exists()).toBe(false)
    expect(accountRow(wrapper, 'Fixture Gamma').find('button').exists()).toBe(false)
    expectNoReinclude(wrapper)

    await button.trigger('click')

    expect(wrapper.emitted('toggleReinclude')).toEqual([[101]])
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(excludedAccountIds).toEqual([101])
    expect(reincludeAccountIds).toEqual([])
  })

  it('renders pending reinclusion and emits the same account id when undoing', async () => {
    const excludedAccountIds = [101, 202]
    const reincludeAccountIds = [202]
    const wrapper = mountSelector({ scopeLocked: true, excludedAccountIds, reincludeAccountIds })
    const pendingRow = accountRow(wrapper, 'Fixture Beta')

    expect(pendingRow.text()).toContain('待重新纳入')
    expect(pendingRow.text()).not.toContain('已排除')
    expect(pendingRow.get('button').text()).toBe('撤销')
    expect(pendingRow.get('button').attributes('type')).toBe('button')
    expect(accountRow(wrapper, 'Fixture Alpha').get('button').text()).toBe('重新纳入')
    expectNoReinclude(wrapper)

    await pendingRow.get('button').trigger('click')

    expect(wrapper.emitted('toggleReinclude')).toEqual([[202]])
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(excludedAccountIds).toEqual([101, 202])
    expect(reincludeAccountIds).toEqual([202])
  })

  it('follows parent draft changes without changing the account selection', async () => {
    const wrapper = mountSelector({ scopeLocked: true, excludedAccountIds: [101], reincludeAccountIds: [] })
    await accountRow(wrapper, 'Fixture Alpha').get('button').trigger('click')
    await wrapper.setProps({ reincludeAccountIds: [101] })

    expect(accountRow(wrapper, 'Fixture Alpha').text()).toContain('待重新纳入')
    expect(accountRow(wrapper, 'Fixture Alpha').get('button').text()).toBe('撤销')
    await accountRow(wrapper, 'Fixture Alpha').get('button').trigger('click')
    await wrapper.setProps({ reincludeAccountIds: [] })

    expect(accountRow(wrapper, 'Fixture Alpha').text()).toContain('已排除')
    expect(accountRow(wrapper, 'Fixture Alpha').get('button').text()).toBe('重新纳入')
    expect(wrapper.emitted('toggleReinclude')).toEqual([[101], [101]])
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })

  it('ignores pending ids that are not excluded from the edited template', () => {
    const wrapper = mountSelector({ scopeLocked: true, excludedAccountIds: [101], reincludeAccountIds: [202] })
    const row = accountRow(wrapper, 'Fixture Beta')

    expect(row.text()).not.toContain('待重新纳入')
    expect(row.find('button').exists()).toBe(false)
    expectNoReinclude(wrapper)
  })

  it('shows a conflicting rule reason and disables explicit reinclusion', async () => {
    const reason = 'Fixture account already has a rule of this kind'
    const wrapper = mountSelector({
      scopeLocked: true,
      excludedAccountIds: [101, 202],
      disabledReason: account => account.id === 101 ? reason : '',
    })
    const row = accountRow(wrapper, 'Fixture Alpha')
    const button = row.get<HTMLButtonElement>('button')

    expect(row.text()).toContain(reason)
    expect(row.text()).toContain('已排除')
    expect(button.text()).toBe('重新纳入')
    expect(button.element.disabled).toBe(true)
    await button.trigger('click')
    expectNoReinclude(wrapper)
    expect(accountRow(wrapper, 'Fixture Beta').get<HTMLButtonElement>('button').element.disabled).toBe(false)
  })

  it('still allows undo when a pending account acquires a conflicting rule', async () => {
    const wrapper = mountSelector({ scopeLocked: true, excludedAccountIds: [101], reincludeAccountIds: [101] })
    const reason = 'Fixture rule appeared after selecting reinclusion'
    await wrapper.setProps({ disabledReason: () => reason })
    const row = accountRow(wrapper, 'Fixture Alpha')
    const button = row.get<HTMLButtonElement>('[data-testid="quality-reinclude-101"]')

    expect(row.text()).toContain(reason)
    expect(row.text()).toContain('待重新纳入')
    expect(button.text()).toBe('撤销')
    expect(button.element.disabled).toBe(false)
    await button.trigger('click')
    expect(wrapper.emitted('toggleReinclude')).toEqual([[101]])
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })
})

describe('QualityAccountSelector manual selection', () => {
  it.each([
    { scope: 'manual' as const, bulk: false },
    { scope: 'all' as const, bulk: true },
  ])('keeps checkboxes and selection actions for scope=$scope bulk=$bulk', async mode => {
    const wrapper = mountSelector({ ...mode, modelValue: [101] })
    const checkboxes = accountCheckboxes(wrapper)

    expect(checkboxes).toHaveLength(3)
    expect(checkboxes[0].element.checked).toBe(true)
    expect(checkboxes[1].element.checked).toBe(false)
    expect(wrapper.get('[data-testid="quality-select-page"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="quality-select-all"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="quality-clear-selection"]').exists()).toBe(true)

    await checkboxes[1].setValue(true)
    expect(wrapper.emitted('update:modelValue')).toEqual([[[101, 202]]])
    expect(wrapper.emitted('toggleReinclude')).toBeUndefined()
    await wrapper.get('[data-testid="quality-select-page"]').trigger('click')
    await wrapper.get('[data-testid="quality-select-all"]').trigger('click')
    await wrapper.get('[data-testid="quality-clear-selection"]').trigger('click')
    expect(wrapper.emitted('selectPage')).toEqual([[]])
    expect(wrapper.emitted('selectAll')).toEqual([[]])
    expect(wrapper.emitted('clear')).toEqual([[]])
  })

  it('keeps disabledReason visible and blocks only the conflicting manual account', async () => {
    const reason = 'Fixture rule conflict'
    const wrapper = mountSelector({
      scope: 'manual',
      disabledReason: account => account.id === 101 ? reason : '',
    })
    const checkboxes = accountCheckboxes(wrapper)

    expect(accountRow(wrapper, 'Fixture Alpha').text()).toContain(reason)
    expect(checkboxes[0].element.disabled).toBe(true)
    expect(checkboxes[1].element.disabled).toBe(false)
    await checkboxes[1].setValue(true)
    expect(wrapper.emitted('update:modelValue')).toEqual([[[202]]])
    expect(wrapper.emitted('toggleReinclude')).toBeUndefined()
  })

  it('keeps manual checkboxes disabled while matching accounts are being selected', () => {
    const wrapper = mountSelector({ scope: 'manual', selectingAccounts: true })

    expect(accountCheckboxes(wrapper)).toHaveLength(3)
    expect(accountCheckboxes(wrapper).every(checkbox => checkbox.element.disabled)).toBe(true)
    expectNoReinclude(wrapper)
  })
})
