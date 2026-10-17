import { createApp } from 'vue'
import naive, { darkTheme } from 'naive-ui'
import App from './App.vue'
import i18n from './i18n'

// Initialize Wails v3 compatibility shim
import './wails-shim.js'

const app = createApp(App)

app.use(naive)
app.use(i18n)

// 挂载全局消息 API
const meta = document.createElement('meta')
meta.name = 'naive-ui-style'
document.head.appendChild(meta)

app.mount('#app')

// 创建全局消息实例（带暗色主题）
import { createDiscreteApi } from 'naive-ui'
const { message } = createDiscreteApi(
  ['message'],
  {
    configProviderProps: {
      theme: darkTheme
    }
  }
)
window.$message = message
