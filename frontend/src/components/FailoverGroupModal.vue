<template>
  <n-modal
    v-model:show="showModal"
    preset="card"
    :title="isEdit ? t('failover.editGroup') : t('failover.addGroup')"
    style="width: 560px;"
    :mask-closable="false"
  >
    <n-form ref="formRef" :model="form" :rules="rules" label-placement="left" label-width="120px">
      <n-form-item :label="t('failover.groupName')" path="name">
        <n-input v-model:value="form.name" :placeholder="t('failover.groupNamePlaceholder')" />
      </n-form-item>

      <n-form-item :label="t('failover.modelId')" path="model">
        <n-input v-model:value="form.model" :placeholder="t('failover.modelIdPlaceholder')" />
        <template #feedback>
          <span style="color: #888; font-size: 12px;">{{ t('failover.modelIdTip') }}</span>
        </template>
      </n-form-item>

      <n-form-item :label="t('failover.cooldown')" path="cooldown">
        <n-input-number v-model:value="form.cooldown" :min="1" :step="10" style="width: 200px;">
          <template #suffix>{{ t('failover.seconds') }}</template>
        </n-input-number>
        <template #feedback>
          <span style="color: #888; font-size: 12px;">{{ t('failover.cooldownTip') }}</span>
        </template>
      </n-form-item>

      <n-form-item v-if="isEdit" :label="t('failover.enabled')">
        <n-switch v-model:value="form.enabled" />
      </n-form-item>
    </n-form>

    <template #footer>
      <n-space justify="end">
        <n-button @click="showModal = false">{{ t('common.cancel') }}</n-button>
        <n-button type="primary" :loading="saving" @click="handleSubmit">
          {{ t('common.save') }}
        </n-button>
      </n-space>
    </template>
  </n-modal>
</template>

<script setup>
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'

const { t } = useI18n()

const props = defineProps({
  visible: { type: Boolean, default: false },
  group: { type: Object, default: null }
})
const emit = defineEmits(['update:visible', 'saved'])

const showModal = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v)
})

const isEdit = computed(() => !!(props.group && props.group.id))
const formRef = ref(null)
const saving = ref(false)
const form = ref({ name: '', model: '', cooldown: 600, enabled: true })

const rules = computed(() => ({
  name: { required: true, message: t('failover.groupNamePlaceholder'), trigger: ['blur', 'input'] },
  model: { required: true, message: t('failover.modelIdPlaceholder'), trigger: ['blur', 'input'] }
}))

watch(() => props.visible, (v) => {
  if (!v) return
  if (props.group) {
    form.value = {
      name: props.group.name || '',
      model: props.group.model || '',
      cooldown: props.group.cooldown_seconds || 600,
      enabled: props.group.enabled !== false
    }
  } else {
    form.value = { name: '', model: '', cooldown: 600, enabled: true }
  }
  formRef.value?.restoreValidation()
})

const handleSubmit = async () => {
  try {
    await formRef.value?.validate()
  } catch (e) {
    return
  }
  saving.value = true
  try {
    if (isEdit.value) {
      await window.go.main.App.UpdateFailoverGroup(
        props.group.id, form.value.name, form.value.model, form.value.cooldown, form.value.enabled
      )
    } else {
      await window.go.main.App.AddFailoverGroup(form.value.name, form.value.model, form.value.cooldown)
    }
    emit('saved')
    showModal.value = false
  } catch (e) {
    window.$message?.error?.(String(e))
  } finally {
    saving.value = false
  }
}
</script>
