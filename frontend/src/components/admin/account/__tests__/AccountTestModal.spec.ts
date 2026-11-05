import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import AccountTestModal from '../AccountTestModal.vue'

const { getAvailableModels, copyToClipboard } = vi.hoisted(() => ({
  getAvailableModels: vi.fn(),
  copyToClipboard: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getAvailableModels
    }
  }
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  const messages: Record<string, string> = {
    'admin.accounts.testPrompt': 'Prompt: "{prompt}"',
    'admin.accounts.imagePromptDefault': 'Generate a cute orange cat astronaut sticker on a clean pastel background.'
  }
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string | number>) => {
        if (key === 'admin.accounts.imageReceived' && params?.count) {
          return `received-${params.count}`
        }
        if (key === 'admin.accounts.imagePreviewAlt' && params?.index) {
          return `test-image-${params.index}`
        }
        return (messages[key] || key).replace(/\{(\w+)\}/g, (_, name) => String(params?.[name] ?? ''))
      }
    })
  }
})

function createStreamResponse(lines: string[]) {
  const encoder = new TextEncoder()
  const chunks = lines.map((line) => encoder.encode(line))
  let index = 0

  return {
    ok: true,
    body: {
      getReader: () => ({
        read: vi.fn().mockImplementation(async () => {
          if (index < chunks.length) {
            return { done: false, value: chunks[index++] }
          }
          return { done: true, value: undefined }
        })
      })
    }
  } as Response
}

function mountModal(account: Record<string, unknown> = {
  id: 42,
  name: 'Gemini Image Test',
  platform: 'gemini',
  type: 'apikey',
  status: 'active'
}) {
  return mount(AccountTestModal, {
    props: {
      show: false,
      account
    } as any,
    global: {
      stubs: {
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
        Select: { template: '<div class="select-stub"></div>' },
        TextArea: {
          props: ['modelValue'],
          emits: ['update:modelValue'],
          methods: { focus: vi.fn(), select: vi.fn() },
          template: '<textarea class="textarea-stub" :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />'
        },
        Icon: true
      }
    }
  })
}

describe('AccountTestModal', () => {
  beforeEach(() => {
    getAvailableModels.mockResolvedValue([
      { id: 'gemini-2.0-flash', display_name: 'Gemini 2.0 Flash' },
      { id: 'gemini-2.5-flash-image', display_name: 'Gemini 2.5 Flash Image' },
      { id: 'gemini-3.1-flash-image', display_name: 'Gemini 3.1 Flash Image' }
    ])
    copyToClipboard.mockReset()
    Object.defineProperty(globalThis, 'localStorage', {
      value: {
        getItem: vi.fn((key: string) => (key === 'auth_token' ? 'test-token' : null)),
        setItem: vi.fn(),
        removeItem: vi.fn(),
        clear: vi.fn()
      },
      configurable: true
    })
    global.fetch = vi.fn().mockResolvedValue(
      createStreamResponse([
        'data: {"type":"test_start","model":"gemini-2.5-flash-image"}\n',
        'data: {"type":"image","image_url":"data:image/png;base64,QUJD","mime_type":"image/png"}\n',
        'data: {"type":"test_complete","success":true}\n'
      ])
    ) as any
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('gemini 图片模型测试会携带提示词并渲染图片预览', async () => {
    const wrapper = mountModal()
    await wrapper.setProps({ show: true })
    await flushPromises()

    expect(wrapper.find('#account-test-inject-default-instructions').exists()).toBe(false)
    const promptInput = wrapper.find('textarea.textarea-stub')
    expect(promptInput.exists()).toBe(true)
    await promptInput.setValue('draw a tiny orange cat astronaut')

    const buttons = wrapper.findAll('button')
    const startButton = buttons.find((button) => button.text().includes('admin.accounts.startTest'))
    expect(startButton).toBeTruthy()

    await startButton!.trigger('click')
    await flushPromises()
    await flushPromises()

    expect(global.fetch).toHaveBeenCalledTimes(1)
    const [, request] = (global.fetch as any).mock.calls[0]
    expect(JSON.parse(request.body)).toEqual({
      model_id: 'gemini-3.1-flash-image',
      prompt: 'draw a tiny orange cat astronaut'
    })

    const preview = wrapper.find('img[alt="test-image-1"]')
    expect(preview.exists()).toBe(true)
    expect(preview.attributes('src')).toBe('data:image/png;base64,QUJD')
  })

  it.each([
    { label: 'API Key', type: 'apikey' },
    { label: 'OAuth', type: 'oauth' }
  ])('OpenAI $label 账号原样预览 SSE image event 中的 GIF 并完成测试', async ({ label, type }) => {
    const model = 'gpt-image-2.6-flare'
    const imageUrl = 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7'
    const prompt = 'draw a tiny orange cat astronaut'
    getAvailableModels.mockResolvedValue([{ id: model, display_name: 'GPT Image 2.6 Flare' }])
    global.fetch = vi.fn().mockResolvedValue(
      createStreamResponse([
        `data: ${JSON.stringify({ type: 'test_start', model })}\n\n`,
        `data: ${JSON.stringify({ type: 'image', image_url: imageUrl, mime_type: 'image/gif' })}\n\n`,
        'data: {"type":"test_complete","success":true}\n\n'
      ])
    ) as any

    const wrapper = mountModal({
      id: 42,
      name: `OpenAI ${label}`,
      platform: 'openai',
      type,
      status: 'active'
    })
    try {
      await wrapper.setProps({ show: true })
      await flushPromises()
      expect(wrapper.find('img[alt="test-image-1"]').exists()).toBe(false)
      expect(wrapper.text()).not.toContain('admin.accounts.testCompleted')
      await wrapper.get('textarea.textarea-stub').setValue(prompt)

      const start = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.startTest'))!
      await start.trigger('click')
      await flushPromises()

      expect(global.fetch).toHaveBeenCalledTimes(1)
      const [url, request] = (global.fetch as any).mock.calls[0]
      expect(url).toMatch(/\/admin\/accounts\/42\/test$/)
      expect(request.method).toBe('POST')
      expect(JSON.parse(request.body)).toEqual({ model_id: model, prompt, mode: 'default' })
      expect(wrapper.get('img[alt="test-image-1"]').attributes('src')).toBe(imageUrl)
      expect(wrapper.text()).toContain('received-1')
      expect(wrapper.text()).toContain('admin.accounts.testCompleted')
    } finally {
      wrapper.unmount()
    }
  })

  it('grok 账号测试默认选择 Grok 模型', async () => {
    getAvailableModels.mockResolvedValue([
      { id: 'grok-4.3', display_name: 'Grok 4.3' },
      { id: 'grok-build-0.1', display_name: 'Grok Build 0.1' }
    ])
    global.fetch = vi.fn().mockResolvedValue(
      createStreamResponse([
        'data: {"type":"test_start","model":"grok-4.3"}\n',
        'data: {"type":"content","text":"ok"}\n',
        'data: {"type":"test_complete","success":true}\n'
      ])
    ) as any

    const wrapper = mountModal({
      id: 13,
      name: 'Grok Account',
      platform: 'grok',
      type: 'oauth',
      status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    const buttons = wrapper.findAll('button')
    const startButton = buttons.find((button) => button.text().includes('admin.accounts.startTest'))
    expect(startButton).toBeTruthy()

    await startButton!.trigger('click')
    await flushPromises()

    expect(global.fetch).toHaveBeenCalledTimes(1)
    const [, request] = (global.fetch as any).mock.calls[0]
    expect(JSON.parse(request.body)).toEqual({
      model_id: 'grok-4.3',
      prompt: 'ciallo',
      mode: 'text'
    })
  })

  it('OpenAI 常规文本测试默认发送 false，点击真实开关后发送 true 且开始测试不重置选择', async () => {
    getAvailableModels.mockResolvedValue([{ id: 'gpt-5.4', display_name: 'GPT-5.4' }])
    global.fetch = vi.fn().mockImplementation(async () =>
      createStreamResponse(['data: {"type":"test_complete","success":true}\n'])
    ) as any
    const wrapper = mountModal({
      id: 42, name: 'OpenAI OAuth', platform: 'openai', type: 'oauth', status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    const toggle = wrapper.get('#account-test-inject-default-instructions[role="switch"]')
    expect(toggle.attributes('aria-checked')).toBe('false')
    const start = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.startTest'))!
    await start.trigger('click')
    await flushPromises()
    expect(JSON.parse((global.fetch as any).mock.calls[0][1].body)).toMatchObject({
      model_id: 'gpt-5.4', mode: 'default', inject_default_instructions: false
    })

    await toggle.trigger('click')
    expect(toggle.attributes('aria-checked')).toBe('true')
    const retry = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.retry'))!
    await retry.trigger('click')
    await flushPromises()
    expect(global.fetch).toHaveBeenCalledTimes(2)
    expect(JSON.parse((global.fetch as any).mock.calls[1][1].body)).toMatchObject({
      model_id: 'gpt-5.4', mode: 'default', inject_default_instructions: true
    })
    expect(toggle.attributes('aria-checked')).toBe('true')
    wrapper.unmount()
  })

  it('OpenAI SIWC OAuth 测试隐藏内置提示词开关且不携带开关值', async () => {
    getAvailableModels.mockResolvedValue([{ id: 'gpt-5.4', display_name: 'GPT-5.4' }])
    global.fetch = vi.fn().mockResolvedValue(
      createStreamResponse(['data: {"type":"test_complete","success":true}\n'])
    ) as any
    const wrapper = mountModal({
      id: 42, name: 'OpenAI SIWC OAuth', platform: 'openai', type: 'oauth', status: 'active',
      extra: { openai_siwc: true }
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    expect(wrapper.find('#account-test-inject-default-instructions').exists()).toBe(false)
    const start = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.startTest'))!
    await start.trigger('click')
    await flushPromises()

    expect(global.fetch).toHaveBeenCalledTimes(1)
    const [, request] = (global.fetch as any).mock.calls[0]
    const body = JSON.parse(request.body)
    expect(body).toEqual({ model_id: 'gpt-5.4', prompt: 'ciallo', mode: 'default' })
    expect(body).not.toHaveProperty('inject_default_instructions')
    wrapper.unmount()
  })

  it('OpenAI 内置提示词开关在重新打开或更换账号后恢复关闭', async () => {
    getAvailableModels.mockResolvedValue([{ id: 'gpt-5.4', display_name: 'GPT-5.4' }])
    const account = {
      id: 42, name: 'OpenAI OAuth', platform: 'openai', type: 'oauth', status: 'active'
    }
    const wrapper = mountModal(account)
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.get('#account-test-inject-default-instructions').trigger('click')
    expect(wrapper.get('#account-test-inject-default-instructions').attributes('aria-checked')).toBe('true')
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(wrapper.get('#account-test-inject-default-instructions').attributes('aria-checked')).toBe('false')

    await wrapper.get('#account-test-inject-default-instructions').trigger('click')
    expect(wrapper.get('#account-test-inject-default-instructions').attributes('aria-checked')).toBe('true')
    await wrapper.setProps({ account: { ...account, id: 43, name: 'Another OpenAI Account' } } as any)
    await flushPromises()
    expect(wrapper.get('#account-test-inject-default-instructions').attributes('aria-checked')).toBe('false')
    wrapper.unmount()
  })

  it.each([
    { label: 'Compact', mode: 'compact', model: 'gpt-5.4', prompt: 'ciallo' },
    { label: '图片', mode: 'default', model: 'gpt-image-1', prompt: 'draw a tiny orange cat astronaut' }
  ])('OpenAI $label 测试隐藏内置提示词开关且不携带开关值', async ({ mode, model, prompt }) => {
    getAvailableModels.mockResolvedValue([
      { id: 'gpt-5.4', display_name: 'GPT-5.4' },
      { id: 'gpt-image-1', display_name: 'GPT Image 1' }
    ])
    global.fetch = vi.fn().mockResolvedValue(
      createStreamResponse([
        'data: {"type":"test_complete","success":true}\n'
      ])
    ) as any

    const wrapper = mountModal({
      id: 42,
      name: 'OpenAI OAuth',
      platform: 'openai',
      type: 'oauth',
      status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.get('#account-test-inject-default-instructions[role="switch"]').trigger('click')
    ;(wrapper.vm as any).selectedModelId = model
    ;(wrapper.vm as any).testMode = mode
    await flushPromises()
    expect(wrapper.find('#account-test-inject-default-instructions').exists()).toBe(false)
    if (model === 'gpt-image-1') {
      await wrapper.get('textarea.textarea-stub').setValue(prompt)
    }
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(global.fetch).toHaveBeenCalledTimes(1)
    const [, request] = (global.fetch as any).mock.calls[0]
    expect(JSON.parse(request.body)).toEqual({
      model_id: model,
      prompt,
      mode
    })
    wrapper.unmount()
  })

  it.each([
    ['  say hello from the connection test  ', 'say hello from the connection test'],
    ['   ', 'ciallo']
  ])('点击提示词后发送实际编辑内容，留空回退默认值 (%j)', async (input, expectedPrompt) => {
    getAvailableModels.mockResolvedValue([{ id: 'claude-sonnet-4-5', display_name: 'Sonnet' }])
    global.fetch = vi.fn().mockResolvedValue(
      createStreamResponse(['data: {"type":"test_complete","success":true}\n'])
    ) as any
    const wrapper = mountModal({
      id: 42, name: 'Claude Account', platform: 'anthropic', type: 'oauth', status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    const editPrompt = wrapper.get('button[aria-controls="account-text-test-prompt-editor"]')
    expect(editPrompt.text()).toContain('Prompt: "ciallo"')
    expect(wrapper.find('textarea').exists()).toBe(false)
    await editPrompt.trigger('click')
    await wrapper.get('textarea').setValue(input)
    expect(editPrompt.text()).toContain(`Prompt: "${expectedPrompt}"`)

    const start = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.startTest'))!
    await start.trigger('click')
    await flushPromises()
    expect(JSON.parse((global.fetch as any).mock.calls[0][1].body)).toMatchObject({
      model_id: 'claude-sonnet-4-5', prompt: expectedPrompt
    })

    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(editPrompt.text()).toContain('Prompt: "ciallo"')
    expect(wrapper.find('textarea').exists()).toBe(false)
    wrapper.unmount()
  })
})


describe('Excel authorization model errors', () => {
  it('shows the authorization state, blocks an empty test and allows reloading', async () => {
    getAvailableModels.mockReset()
    getAvailableModels.mockRejectedValueOnce({reason: 'OPENAI_EXCEL_AUTH_VERIFICATION_REQUIRED', message: 'private error must not be rendered'})
    const wrapper = mountModal()
    await wrapper.setProps({show: true})
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('admin.accounts.excelAuthErrors.OPENAI_EXCEL_AUTH_VERIFICATION_REQUIRED')
    expect(wrapper.text()).not.toContain('private error')
    expect(wrapper.get('[role="alert"] a').attributes('href')).toBe('/admin/token-guard-v2')
    const start = wrapper.findAll('button').find(b => b.text().includes('admin.accounts.startTest'))!
    expect(start.attributes('disabled')).toBeDefined()
    getAvailableModels.mockResolvedValueOnce([{id:'gpt-6-astra',display_name:'Astra'}])
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(start.attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
})


it('keeps native model testing available during automatic BPS authorization', async () => {
  getAvailableModels.mockResolvedValue([{id:'gpt-6-sol',display_name:'Sol'}])
  const wrapper = mountModal({id:42,name:'Automatic BPS',platform:'openai',type:'oauth',status:'active',extra:{openai_excel_bps:true,openai_excel_bps_authorization_pending:true}})
  await wrapper.setProps({show:true})
  await flushPromises()
  expect(wrapper.get('[role="status"]').text()).toContain('admin.accounts.bpsAuthorizing')
  const start=wrapper.findAll('button').find(b=>b.text().includes('admin.accounts.startTest'))!
  expect(start.attributes('disabled')).toBeUndefined()
  wrapper.unmount()
})
