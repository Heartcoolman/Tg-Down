export interface Stats {
  total: number
  downloaded: number
  failed: number
  skipped: number
  total_size: number
  downloaded_size: number
}

export interface MediaProgress {
  id: string
  task_id?: string
  message_id?: number
  td_file_id?: number
  chat_id?: number
  file_name: string
  media_type: string
  file_size: number
  downloaded_size: number
  percent?: number
  status: string
  paused?: boolean
  file_path?: string
  started_at?: string
  updated_at?: string
  speed_bps?: number
  eta_seconds?: number
}

export interface HistoryFilters {
  media_types?: string[]
  date_from?: number
  date_to?: number
  max_file_size?: number
  query?: string
  sender_id?: number
}

export interface Task {
  id: string
  kind: string
  chat_id: number
  chat_title?: string
  status: string
  error?: string
  created_at: string
  started_at?: string
  finished_at?: string
  stats: Stats
  phase?: string
  expected_total?: number
  scanned_messages?: number
  found_media?: number
  scan_cursor?: number
  speed_bps?: number
  eta_seconds?: number
  attempts?: number
  filters?: HistoryFilters | null
  message_id?: number
}

export interface Chat {
  id: number
  title: string
  type?: string
  unread?: number
}

export interface LogEntry {
  time: string
  level: string
  msg: string
}

export interface StateSnapshot {
  version: string
  state: string
  error?: string
  phone?: string
  target_chat: number
  active_tasks: number
  stats: Stats
  media: MediaProgress[]
  media_concurrency?: DownloadSettings
  all_paused?: boolean
  speed_bps?: number
  connection?: string
}

export interface HistoryRecord {
  id: number
  task_id?: string
  chat_id: number
  chat_title?: string
  message_id: number
  media_type: string
  file_name: string
  file_path: string
  file_size: number
  mime_type?: string
  status: string
  reason?: string
  created_at: number
  finished_at?: number
  album_id?: number
  has_thumb: boolean
}

export interface HistoryPage {
  items: HistoryRecord[]
  /** 下一页的不透明游标；空表示已到末页。前端只负责原样回传 */
  next_cursor?: string
  /** 仅在请求带 with_total=1 时返回 */
  total?: number
  limit: number
}

export interface MediaTypeStat {
  media_type: string
  count: number
  total_size: number
  completed: number
  failed: number
  skipped: number
}

export interface HistoryStatsResponse {
  by_type: MediaTypeStat[]
}

export interface DownloadSettings {
  max_concurrent: number
  active: number
}

// —— 时间线（类 Telegram 本地浏览）——

export interface TimelineChat {
  chat_id: number
  title: string
  count: number
  last_date: number
  last_date_text?: string
}

export interface TimelineEntry {
  message_id: number
  chat_id: number
  chat_title?: string
  date: number
  date_text?: string
  sender_id?: number
  caption?: string
  album_id?: number
  media_type: string
  file_name: string
  file_size?: number
  mime_type?: string
  unique_id?: string
  message_url?: string
  rel_path: string
}

export interface TimelinePage {
  chats: TimelineChat[]
  items: TimelineEntry[]
  /** 下一页游标（原样回传 before_date/before_msg_id）；空 = 末页 */
  next_cursor?: string
  limit: number
  built_at: number
}

export interface OKResponse {
  status: 'ok'
}

export interface ResolvedTarget {
  chat_id: number
  chat_title?: string
  message_id?: number
}

export interface Schedule {
  id: string
  chat_id: number
  chat_title?: string
  interval_min: number
  filters?: string
  enabled: boolean
  last_run?: string
  created_at: string
  last_max_id?: number
}

export interface Settings {
  download_path: string
  classify_by_type: boolean
  media_concurrency: DownloadSettings
  save_metadata?: boolean
  path_template?: string
  proxy?: string
  task_concurrency?: number
  auto_retry?: number
  log_level?: string
  notify_telegram_self?: boolean
  notify_webhook_url?: string
}

export interface SettingsUpdate {
  classify_by_type?: boolean
  path_template?: string
  max_concurrent?: number
  save_metadata?: boolean
  proxy?: string | null
  task_concurrency?: number
  auto_retry?: number
  log_level?: string
  notify_telegram_self?: boolean
  notify_webhook_url?: string | null
}

export interface SettingsUpdateResponse {
  settings: Settings
  notices?: string[]
}

// ---- 桌面壳（/desktop/api/*）----

export interface DesktopInfo {
  version: string
  goos: string
  goarch: string
  app_dir: string
  autostart: boolean
  /** 非空表示自启状态查询失败，与"未启用"不是一回事 */
  autostart_error?: string
  local: string
}

export interface AutostartState {
  enabled: boolean
}

export interface RemoteInstance {
  id: string
  name: string
  url: string
  has_token: boolean
}

export interface InstancesResponse {
  selected: string
  instances: RemoteInstance[]
}

export interface TestResult {
  ok: boolean
  version?: string
  state?: string
  error?: string
}

export interface ExportResult {
  json_path: string
  html_path: string
  messages: number
  media_count: number
  media_on_disk: number
  truncated: boolean
  chat_title?: string
  exported_at?: number
}
