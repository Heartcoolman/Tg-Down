import type {
  Chat, DownloadSettings, ExportResult, HistoryFilters, HistoryPage, HistoryStatsResponse,
  OKResponse, ResolvedTarget, Schedule, Settings, SettingsUpdate, SettingsUpdateResponse,
  StateSnapshot, Task, TimelinePage,
} from './types'
import { apiBase } from './desktop'

const TOKEN_KEY = 'tg_down_token'
const COOKIE_AUTH_MARKER = 'tg_down_web_cookie_auth=1'

function supportsCookieAuth(): boolean {
  return document.cookie.split(';').some((part) => part.trim() === COOKIE_AUTH_MARKER)
}

type AuthInit = { token: string; redirecting: boolean }

// 新版后端会在页面响应写入固定能力标记。检测到旧 localStorage 令牌时，立即删除原文，
// 只导航一次让后端换成 HttpOnly Cookie。旧后端没有标记，才保留 Bearer/查询参数兼容。
function initializeAuth(): AuthInit {
  const fromURL = new URLSearchParams(location.search).get('token')
  if (fromURL) {
    // 新后端会在 HTML 发送前截获 token，因此 JS 能看到它说明当前仍是旧后端。
    if (supportsCookieAuth()) {
      try { localStorage.removeItem(TOKEN_KEY) } catch { /* 隐私模式下不可用，忽略 */ }
      const clean = new URL(location.href)
      clean.searchParams.delete('token')
      history.replaceState(history.state, '', clean.pathname + clean.search + clean.hash)
      return { token: '', redirecting: false }
    }
    try { localStorage.setItem(TOKEN_KEY, fromURL) } catch { /* 隐私模式下不可用，忽略 */ }
    const clean = new URL(location.href)
    clean.searchParams.delete('token')
    history.replaceState(history.state, '', clean.pathname + clean.search + clean.hash)
    return { token: fromURL, redirecting: false }
  }

  let stored = ''
  try { stored = localStorage.getItem(TOKEN_KEY) || '' } catch { /* 隐私模式下不可用，忽略 */ }
  if (stored && supportsCookieAuth()) {
    try { localStorage.removeItem(TOKEN_KEY) } catch { /* 隐私模式下不可用，忽略 */ }
    const target = new URL(location.href)
    target.searchParams.set('token', stored)
    location.replace(target.toString())
    return { token: '', redirecting: true }
  }
  return { token: stored, redirecting: false }
}

const authInit = initializeAuth()
export const authToken = authInit.token
export const authRedirecting = authInit.redirecting

export class APIError extends Error {
  constructor(message: string, readonly status: number) {
    super(message)
    this.name = 'APIError'
  }
}

export function beginTokenBootstrap(token: string) {
  try { localStorage.removeItem(TOKEN_KEY) } catch { /* 隐私模式下不可用，忽略 */ }
  const target = new URL(location.href)
  target.searchParams.set('token', token)
  location.replace(target.toString())
}

// withToken 附加当前实例的 API 基路径；本地令牌仅在直连部署（网页端）场景存在。
// 桌面远程实例的令牌由壳层反代注入，不经过这里。
export function withToken(path: string): string {
  const base = apiBase()
  const full = base + path
  if (!authToken) return full
  return full + (full.includes('?') ? '&' : '?') + 'token=' + encodeURIComponent(authToken)
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  if (authToken) headers.set('Authorization', 'Bearer ' + authToken)
  if (init?.body) headers.set('Content-Type', 'application/json')

  const res = await fetch(path, { ...init, headers, credentials: 'same-origin' })
  const text = await res.text()
  let data: unknown = null
  if (text) {
    try { data = JSON.parse(text) } catch { data = null }
  }
  if (!res.ok) {
    const msg = (data as { error?: string } | null)?.error
    throw new APIError(msg || `HTTP ${res.status}`, res.status)
  }
  return data as T
}

const get = <T,>(path: string) => request<T>(path)
const post = <T,>(path: string, body?: unknown) =>
  request<T>(path, { method: 'POST', body: body === undefined ? undefined : JSON.stringify(body) })

export const api = {
  state: () => get<StateSnapshot>(withToken('/api/state')),
  chats: () => get<Chat[]>(withToken('/api/chats')),
  refreshChats: () => post<Chat[]>(withToken('/api/chats/refresh')),
  settings: () => get<Settings>(withToken('/api/settings')),
  updateSettings: (patch: SettingsUpdate) =>
    post<SettingsUpdateResponse>(withToken('/api/settings'), patch),
  setClassify: (v: boolean) => post<Settings>(withToken('/api/settings/classify'), { classify_by_type: v }),

  submitCredentials: (api_id: number, api_hash: string, phone: string) =>
    post<OKResponse>(withToken('/api/auth/credentials'), { api_id, api_hash, phone }),
  submitCode: (code: string) => post<OKResponse>(withToken('/api/auth/code'), { code }),
  submitPassword: (password: string) => post<OKResponse>(withToken('/api/auth/password'), { password }),
  abortAuth: () => post<OKResponse>(withToken('/api/auth/abort')),
  logout: () => post<OKResponse>(withToken('/api/auth/logout')),

  tasks: () => get<Task[]>(withToken('/api/tasks')),
  createTask: (body: { kind: string; chat_id: number; chat_title?: string; filters?: HistoryFilters; message_id?: number }) =>
    post<Task>(withToken('/api/tasks'), body),
  cancelTask: (id: string) => post<OKResponse>(withToken(`/api/tasks/${id}/cancel`)),
  retryTask: (id: string) => post<Task>(withToken(`/api/tasks/${id}/retry`)),
  resolve: (input: string) => post<ResolvedTarget>(withToken('/api/resolve'), { input }),

  setConcurrency: (n: number) => post<DownloadSettings>(withToken('/api/download/concurrency'), { max_concurrent: n }),
  pauseMedia: (id: string) => post<OKResponse>(withToken(`/api/media/${encodeURIComponent(id)}/pause`)),
  resumeMedia: (id: string) => post<OKResponse>(withToken(`/api/media/${encodeURIComponent(id)}/resume`)),
  pauseAll: () => post<OKResponse>(withToken('/api/media/pause-all')),
  resumeAll: () => post<OKResponse>(withToken('/api/media/resume-all')),

  history: (params: URLSearchParams) => get<HistoryPage>(withToken('/api/history?' + params.toString())),
  historyStats: (params: URLSearchParams) => get<HistoryStatsResponse>(withToken('/api/history/stats?' + params.toString())),
  deleteHistory: (id: number) =>
    request<OKResponse>(withToken(`/api/history/${id}`), { method: 'DELETE' }),
  clearTaskHistory: (id: string) =>
    request<{ deleted: number }>(withToken(`/api/tasks/${encodeURIComponent(id)}/history`), { method: 'DELETE' }),
  // 导出走浏览器下载而非 fetch：响应带 Content-Disposition，交给浏览器直接落盘，
  // 避免把整份导出读进内存再造一个 blob
  exportHistory: async (params: URLSearchParams) => {
    const url = withToken('/api/history/export?' + params.toString())
    const res = await fetch(url, { credentials: 'same-origin' })
    if (!res.ok) throw new APIError(await res.text() || `HTTP ${res.status}`, res.status)
    const blob = await res.blob()
    const href = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = href
    a.download = (res.headers.get('Content-Disposition') || '').match(/filename="([^"]+)"/)?.[1]
      || 'history.csv'
    document.body.appendChild(a)
    a.click()
    a.remove()
    URL.revokeObjectURL(href)
  },

  schedules: () => get<Schedule[]>(withToken('/api/schedules')),
  createSchedule: (body: { chat_id: number; chat_title?: string; interval_min: number; filters?: HistoryFilters }) =>
    post<Schedule>(withToken('/api/schedules'), body),
  deleteSchedule: (id: string) =>
    request<OKResponse>(withToken(`/api/schedules/${id}`), { method: 'DELETE' }),
  toggleSchedule: (id: string, enabled: boolean) =>
    post<OKResponse>(withToken(`/api/schedules/${id}/toggle`), { enabled }),

  exportChat: (chat_id: number, chat_title: string, limit: number) =>
    post<ExportResult>(withToken('/api/export'), { chat_id, chat_title, limit }),

  timeline: (params: URLSearchParams) =>
    get<TimelinePage>(withToken('/api/timeline?' + params.toString())),
}

// 时间线媒体文件 URL（按相对路径寻址，后端校验防越界）
export const timelineFileURL = (relPath: string) =>
  withToken('/api/timeline/file?path=' + encodeURIComponent(relPath))

// 媒体 URL：按 history id 寻址，后端据此查库拿路径（见 internal/web/media.go）
export const thumbURL = (id: number) => withToken(`/api/history/${id}/thumb`)
export const fileURL = (id: number) => withToken(`/api/history/${id}/file`)
