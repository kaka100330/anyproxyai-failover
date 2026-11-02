import { createApp } from 'vue'
import naive, { darkTheme, createDiscreteApi } from 'naive-ui'
import App from './App.vue'
import i18n from './i18n'

// 桌面模式使用 Wails shim，Web 模式使用 HTTP RPC shim。
// 使用 Promise 而不是顶层 await，兼容 Vite 默认构建目标。
const shimReady = import.meta.env.MODE === 'web'
  ? import('./web-shim.js')
  : import('./wails-shim.js')

shimReady.then(() => {
  const app = createApp(App)

  app.use(naive)
  app.use(i18n)

  // 挂载全局消息 API
  const meta = document.createElement('meta')
  meta.name = 'naive-ui-style'
  document.head.appendChild(meta)

  app.mount('#app')

  // 创建全局消息实例（带暗色主题）
  const { message } = createDiscreteApi(
    ['message'],
    {
      configProviderProps: {
        theme: darkTheme
      }
    }
  )
  window.$message = message
})
