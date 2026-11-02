/**
 * Web 模式兼容层：把桌面版 `window.go.main.App.*` 调用映射到
 * `POST /admin/api/rpc`，因此 App.vue 无需任何改动即可在浏览器中使用。
 */

const rpc = async (method, args) => {
  const res = await fetch('/admin/api/rpc', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
    body: JSON.stringify({ method, args }),
  })
  if (res.status === 401) {
    location.href = '/login'
    throw new Error('登录已过期，请重新登录')
  }
  const data = await res.json().catch(() => ({ ok: false, error: '响应解析失败' }))
  if (!data.ok) {
    throw new Error(data.error || '请求失败')
  }
  return data.result
}

const App = new Proxy(
  {},
  {
    get(_target, method) {
      if (typeof method !== 'string') return undefined
      return (...args) => {
        console.log(`RPC: ${method}`, args)
        return rpc(method, args)
      }
    },
  }
)

window.go = { main: { App } }

console.log('AnyProxyAI web shim initialized')
