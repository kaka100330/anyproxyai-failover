<template>
  <n-modal
    v-model:show="showModal"
    preset="card"
    :title="`${t('failover.endpointsOf')} ${group?.name || ''}  ·  ${group?.model || ''}`"
    style="width: 980px;"
    :mask-closable="false"
  >
    <n-space vertical :size="12">
      <n-alert type="info" :show-icon="true" style="font-size: 13px;">
        {{ t('failover.endpointsTip', { n: group?.cooldown_seconds || 600 }) }}
      </n-alert>

      <n-space justify="end">
        <n-button type="primary" size="small" @click="openEndpointForm(null)">
          <template #icon><n-icon><AddIcon /></n-icon></template>
          {{ t('failover.addEndpoint') }}
        </n-button>
      </n-space>

      <n-data-table
        :columns="columns"
        :data="rows"
        :bordered="false"
        size="small"
        :pagination="false"
        :single-line="false"
        striped
      />

      <n-empty v-if="rows.length === 0" :description="t('failover.noEndpoints')" style="margin: 24px 0;" />
    </n-space>

    <!-- 端点编辑表单 -->
    <n-modal
      v-model:show="showEndpointForm"
      preset="card"
      :title="editingEndpoint ? t('failover.editEndpoint') : t('failover.addEndpoint')"
      style="width: 560px;"
    >
      <n-form ref="epFormRef" :model="epForm" :rules="epRules" label-placement="left" label-width="110px">
        <n-form-item :label="t('failover.endpointName')" path="name">
          <n-input v-model:value="epForm.name" :placeholder="t('failover.endpointNamePlaceholder')" />
        </n-form-item>
        <n-form-item :label="t('addRoute.apiUrl')" path="apiUrl">
          <n-input v-model:value="epForm.apiUrl" :placeholder="t('addRoute.apiUrlPlaceholder')" />
        </n-form-item>
        <n-form-item :label="t('addRoute.apiKey')" path="apiKey">
          <n-input v-model:value="epForm.apiKey" type="password" show-password-on="click" :placeholder="t('addRoute.apiKeyPlaceholder')" />
        </n-form-item>
        <n-form-item :label="t('addRoute.apiFormat')" path="format">
          <n-select v-model:value="epForm.format" :options="formatOptions" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showEndpointForm = false">{{ t('common.cancel') }}</n-button>
          <n-button type="primary" :loading="saving" @click="saveEndpoint">{{ t('common.save') }}</n-button>
        </n-space>
      </template>
    </n-modal>
  </n-modal>
</template>

<script setup>
import { ref, computed, h, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NIcon, NSpace, NTag, NText } from 'naive-ui'
import { Add as AddIcon, ArrowUp, ArrowDown, CreateOutline as EditIcon, TrashOutline as DeleteIcon } from '@vicons/ionicons5'

const { t } = useI18n()

const props = defineProps({
  visible: { type: Boolean, default: false },
  group: { type: Object, default: null }
})
const emit = defineEmits(['update:visible', 'changed'])

const showModal = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v)
})

const showEndpointForm = ref(false)
const epFormRef = ref(null)
const editingEndpoint = ref(null)
const saving = ref(false)
const epForm = ref({ name: '', apiUrl: '', apiKey: '', format: 'openai' })

const formatOptions = [
  { label: 'OpenAI', value: 'openai' },
  { label: 'Claude', value: 'claude' },
  { label: 'Gemini', value: 'gemini' }
]

const epRules = computed(() => ({
  name: { required: true, message: t('failover.endpointNamePlaceholder'), trigger: ['blur', 'input'] },
  apiUrl: { required: true, message: t('addRoute.apiUrlPlaceholder'), trigger: ['blur', 'input'] }
}))

const rows = computed(() => {
  const eps = props.group?.endpoints || []
  return [...eps].sort((a, b) => (a.priority ?? 0) - (b.priority ?? 0) || a.id - b.id)
})

const statusOf = (ep) => {
  const list = props.group?.status?.endpoints || []
  return list.find((s) => s.endpoint_id === ep.id)
}

const maskKey = (key) => {
  if (!key) return '-'
  if (key.length <= 10) return key
  return key.substring(0, 5) + '***' + key.substring(key.length - 5)
}

const columns = computed(() => [
  {
    title: t('failover.priority'),
    key: 'priority',
    width: 120,
    render: (row, index) => h(NSpace, { size: 4, align: 'center', wrap: false }, {
      default: () => [
        h(NTag, { size: 'small', type: index === 0 ? 'success' : 'default' }, { default: () => `#${index + 1}` }),
        h(NButton, { size: 'tiny', quaternary: true, disabled: index === 0, onClick: () => move(row, -1) }, { default: () => h(NIcon, { size: 13 }, { default: () => h(ArrowUp) }) }),
        h(NButton, { size: 'tiny', quaternary: true, disabled: index === rows.value.length - 1, onClick: () => move(row, 1) }, { default: () => h(NIcon, { size: 13 }, { default: () => h(ArrowDown) }) })
      ]
    })
  },
  { title: t('failover.endpointName'), key: 'name', width: 140 },
  { title: t('addRoute.apiUrl'), key: 'api_url', ellipsis: { tooltip: true } },
  { title: t('addRoute.apiKey'), key: 'api_key', width: 130, render: (row) => maskKey(row.api_key) },
  { title: t('addRoute.apiFormat'), key: 'format', width: 90 },
  {
    title: t('failover.status'),
    key: 'status',
    width: 120,
    render: (row) => {
      const st = statusOf(row)
      if (st?.cooling) {
        return h(NTag, { size: 'small', type: 'warning' }, { default: () => `${t('failover.cooling')} ${st.remain_seconds}s` })
      }
      if (props.group?.status?.active_endpoint_id === row.id) {
        return h(NTag, { size: 'small', type: 'success' }, { default: () => t('failover.active') })
      }
      return h(NText, { depth: 3 }, { default: () => '-' })
    }
  },
  {
    title: t('models.actions'),
    key: 'actions',
    width: 150,
    render: (row) => h(NSpace, { size: 4 }, {
      default: () => [
        h(NButton, { size: 'tiny', onClick: () => openEndpointForm(row) }, { default: () => t('models.edit'), icon: () => h(NIcon, { size: 13 }, { default: () => h(EditIcon) }) }),
        h(NButton, { size: 'tiny', type: 'error', onClick: () => removeEndpoint(row) }, { default: () => t('models.delete'), icon: () => h(NIcon, { size: 13 }, { default: () => h(DeleteIcon) }) })
      ]
    })
  }
])

watch(() => props.visible, (v) => {
  if (v) showEndpointForm.value = false
})

const openEndpointForm = (ep) => {
  editingEndpoint.value = ep
  epForm.value = ep
    ? { name: ep.name, apiUrl: ep.api_url, apiKey: ep.api_key, format: ep.format || 'openai' }
    : { name: '', apiUrl: '', apiKey: '', format: 'openai' }
  showEndpointForm.value = true
  setTimeout(() => epFormRef.value?.restoreValidation(), 0)
}

const saveEndpoint = async () => {
  try {
    await epFormRef.value?.validate()
  } catch (e) {
    return
  }
  saving.value = true
  try {
    if (editingEndpoint.value) {
      await window.go.main.App.UpdateFailoverEndpoint(
        editingEndpoint.value.id, epForm.value.name, epForm.value.apiUrl, epForm.value.apiKey,
        epForm.value.format, editingEndpoint.value.priority ?? 0
      )
    } else {
      await window.go.main.App.AddFailoverEndpoint(
        props.group.id, epForm.value.name, epForm.value.apiUrl, epForm.value.apiKey, epForm.value.format
      )
    }
    showEndpointForm.value = false
    emit('changed')
  } catch (e) {
    window.$message?.error?.(String(e))
  } finally {
    saving.value = false
  }
}

const move = async (row, direction) => {
  try {
    await window.go.main.App.MoveFailoverEndpoint(row.id, direction)
    emit('changed')
  } catch (e) {
    window.$message?.error?.(String(e))
  }
}

const removeEndpoint = async (row) => {
  try {
    await window.go.main.App.DeleteFailoverEndpoint(row.id)
    emit('changed')
  } catch (e) {
    window.$message?.error?.(String(e))
  }
}
</script>
