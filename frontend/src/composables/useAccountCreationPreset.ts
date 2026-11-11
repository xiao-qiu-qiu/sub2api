import { onBeforeUnmount, ref, watch } from 'vue'
import { getAccountCreationPreset, saveAccountCreationPreset, type AccountCreationPreset } from '@/api/admin/accountCreationPreset'

export function useAccountCreationPreset(options: {
  context: () => string
  available: () => boolean
  capture: () => AccountCreationPreset
  snapshot: () => string
  apply: (preset: AccountCreationPreset) => void
}) {
  const busy = ref(false), applied = ref(false), status = ref(''), error = ref('')
  let generation = 0
  function reset() { generation++; busy.value = applied.value = false; status.value = error.value = '' }
  watch(options.context, reset, { flush: 'sync' })
  onBeforeUnmount(reset)

  async function run(action: 'load' | 'save') {
    if (busy.value || !options.available()) return
    const current = generation
    busy.value = true; status.value = error.value = ''
    try {
      // Only form settings enter the payload. Identity and credentials never do.
      const snapshot = options.snapshot()
      const preset = action === 'save' ? await saveAccountCreationPreset(options.capture()) : await getAccountCreationPreset()
      if (generation !== current || !options.available()) return
      if (!preset) { status.value = 'empty'; return }
      if (action === 'load') {
        if (snapshot !== options.snapshot()) { status.value = 'changed'; return }
        options.apply(preset)
      }
      applied.value = true
      status.value = action === 'load' ? 'loaded' : 'saved'
    } catch (e) {
      if (generation === current) error.value = e instanceof Error ? e.message : String(e)
    } finally { if (generation === current) busy.value = false }
  }
  return { busy, applied, status, error, reset, load: () => run('load'), save: () => run('save') }
}
