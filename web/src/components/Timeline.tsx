import { useEffect, useRef, useState } from 'preact/hooks'
import { timelineFileURL, api } from '../api'
import { toast } from '../store'
import type { TimelineChat, TimelineEntry, TimelinePage } from '../types'
import { fmtSize } from '../format'

/* ---------- 外观皮肤（localStorage 持久化） ---------- */
const SKIN_KEY = 'tg_down_tl_skin'
const SWATCHES = [
  { cls: 'tl-bg-default' },
  { cls: 'tl-bg-geo' },
  { cls: 'tl-bg-moe' },
  { cls: 'tl-bg-sakura' },
]

interface Skin {
  bg: string
  url: string
  panelAlpha: number
  bubbleAlpha: number
  blur: number
}

function loadSkin(): Skin {
  try {
    const raw = localStorage.getItem(SKIN_KEY)
    if (raw) return { bg: 'tl-bg-default', url: '', panelAlpha: 1, bubbleAlpha: 1, blur: 0, ...JSON.parse(raw) }
  } catch { /* 隐私模式忽略 */ }
  return { bg: 'tl-bg-default', url: '', panelAlpha: 1, bubbleAlpha: 1, blur: 0 }
}

/* ---------- 工具 ---------- */
function fmtClock(ts: number): string {
  return new Date(ts * 1000).toTimeString().slice(0, 5)
}

// 后端按 (date desc, msg_id desc) 返回新→旧；界面按 TG 习惯最新在底、上翻看旧，
// 所以渲染前整体反转成旧→新
function flip(list: TimelineEntry[]): TimelineEntry[] {
  return [...list].reverse()
}

function fmtDay(ts: number): string {
  const d = new Date(ts * 1000)
  const now = new Date()
  if (d.toDateString() === now.toDateString()) return '今天'
  const yest = new Date(now.getTime() - 86400000)
  if (d.toDateString() === yest.toDateString()) return '昨天'
  return `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日`
}

function fmtSync(ts: number): string {
  const d = new Date(ts * 1000)
  const now = new Date()
  if (d.toDateString() === now.toDateString()) return `今天 ${fmtClock(ts)}`
  const yest = new Date(now.getTime() - 86400000)
  if (d.toDateString() === yest.toDateString()) return `昨天 ${fmtClock(ts)}`
  return `${d.getMonth() + 1}月${d.getDate()}日`
}

function avatarHue(id: number): number {
  return ((id * 2654435761) >>> 0) % 360
}

function isGroupType(type?: string): boolean {
  if (!type) return false
  return /group/i.test(type)
}

// tagify：把 caption 里的 #标签 渲染为可点击胶囊（点击走容器上的事件委托；
// 管理台 CSP 是 script-src 'self'，行内 onclick 会被浏览器拦截）
function tagify(text: string): string {
  const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  let out = ''
  let last = 0
  const re = /#[\p{L}\p{N}_]+/gu
  let m: RegExpExecArray | null
  while ((m = re.exec(text)) !== null) {
    out += esc(text.slice(last, m.index))
    const tag = m[0].slice(1)
    out += `<a class="tl-tag" data-tag="${esc(tag)}">${esc(m[0])}</a>`
    last = m.index + m[0].length
  }
  out += esc(text.slice(last))
  return out
}

/* ---------- 媒体 ---------- */
function MediaItem({ entry }: { entry: TimelineEntry }) {
  const url = timelineFileURL(entry.rel_path)
  const isImage = /\.(jpe?g|png|gif|webp)$/i.test(entry.file_name)
  const isVideo = /\.(mp4|mkv|webm|mov)$/i.test(entry.file_name)
  if (isImage) {
    return (
      <a class="tl-media" href={url} target="_blank" rel="noopener noreferrer" title={entry.file_name}>
        <img src={url} alt={entry.caption || entry.file_name} loading="lazy" />
      </a>
    )
  }
  return (
    <a class="tl-media tl-file" href={url} target="_blank" rel="noopener noreferrer" title={entry.file_name}>
      <span class="tl-file-icon">{isVideo ? '🎬' : '📄'}</span>
      <span class="tl-file-name mono">{entry.file_name}</span>
      <span class="tl-file-size">{fmtSize(entry.file_size || 0)}</span>
    </a>
  )
}

/* ---------- 分组 ---------- */
function groupByAlbum(items: TimelineEntry[]): TimelineEntry[][] {
  const groups: TimelineEntry[][] = []
  for (const it of items) {
    const last = groups[groups.length - 1]
    if (last && it.album_id && last[0].album_id === it.album_id) {
      last.push(it)
    } else {
      groups.push([it])
    }
  }
  return groups
}

interface Grouped {
  day: string
  groups: TimelineEntry[][]
  firstTs: number
}

function groupByDay(items: TimelineEntry[]): Grouped[] {
  const out: Grouped[] = []
  for (const it of items) {
    const day = new Date(it.date * 1000).toDateString()
    const last = out[out.length - 1]
    if (last && new Date(last.firstTs * 1000).toDateString() === day) {
      last.groups.push(...groupByAlbum([it]))
    } else {
      out.push({ day, firstTs: it.date, groups: groupByAlbum([it]) })
    }
  }
  return out
}

/* ---------- 外观面板 ---------- */
function SkinPanel({ skin, onApply }: { skin: Skin; onApply: (s: Skin) => void }) {
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState<Skin>(skin)

  useEffect(() => {
    setDraft(skin)
  }, [skin])

  const set = (patch: Partial<Skin>) => setDraft((d) => ({ ...d, ...patch }))

  return (
    <>
      <button class="tl-skin-btn" title="外观设置" onClick={() => setOpen(!open)}>🎨</button>
      {open && (
        <div class="tl-skin-panel" onClick={(e) => e.stopPropagation()}>
          <h4>聊天背景</h4>
          <div class="tl-swatches">
            {SWATCHES.map((s) => (
              <div
                key={s.cls}
                class={`tl-swatch ${s.cls}${draft.bg === s.cls ? ' on' : ''}`}
                title={s.cls}
                onClick={() => set({ bg: s.cls, url: '' })}
              />
            ))}
          </div>
          <div class="tl-skin-row">
            <span>图片 URL</span>
            <input
              type="text" placeholder="https://…（留空清除）" value={draft.url}
              onInput={(e) => set({ url: e.currentTarget.value })}
            />
          </div>
          <h4>组件效果</h4>
          <div class="tl-skin-row">
            <span>面板透明度</span>
            <input
              type="range" min="0.2" max="1" step="0.05" value={draft.panelAlpha}
              onInput={(e) => set({ panelAlpha: parseFloat(e.currentTarget.value) })}
            />
            <span class="tl-skin-val">{Math.round(draft.panelAlpha * 100)}%</span>
          </div>
          <div class="tl-skin-row">
            <span>气泡透明度</span>
            <input
              type="range" min="0.2" max="1" step="0.05" value={draft.bubbleAlpha}
              onInput={(e) => set({ bubbleAlpha: parseFloat(e.currentTarget.value) })}
            />
            <span class="tl-skin-val">{Math.round(draft.bubbleAlpha * 100)}%</span>
          </div>
          <div class="tl-skin-row">
            <span>毛玻璃模糊</span>
            <input
              type="range" min="0" max="30" step="1" value={draft.blur}
              onInput={(e) => set({ blur: parseInt(e.currentTarget.value, 10) })}
            />
            <span class="tl-skin-val">{draft.blur}px</span>
          </div>
          <div class="tl-skin-actions">
            <button
              onClick={() => {
                const def: Skin = { bg: 'tl-bg-default', url: '', panelAlpha: 1, bubbleAlpha: 1, blur: 0 }
                setDraft(def)
                onApply(def)
              }}
            >重置</button>
            <button class="primary" onClick={() => onApply(draft)}>应用到界面</button>
          </div>
        </div>
      )}
    </>
  )
}

/* ---------- 主组件 ---------- */
export function Timeline() {
  const [chats, setChats] = useState<TimelineChat[]>([])
  const [chatTypes, setChatTypes] = useState<Record<number, string>>({})
  const [selected, setSelected] = useState<number>(0)
  const [items, setItems] = useState<TimelineEntry[]>([])
  const [cursor, setCursor] = useState('')
  const [loading, setLoading] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [opened, setOpened] = useState(false)
  const [tags, setTags] = useState<string[]>([])
  const [tagMode, setTagMode] = useState<'or' | 'and'>('or')
  const [skin, setSkin] = useState<Skin>(loadSkin)
  const scrollRef = useRef<HTMLDivElement>(null)
  const cursorRef = useRef('')
  const selectedRef = useRef(0)
  const loadingRef = useRef(false)
  const tagsRef = useRef<string[]>([])
  const tagModeRef = useRef<'or' | 'and'>('or')
  cursorRef.current = cursor
  selectedRef.current = selected
  loadingRef.current = loading
  tagsRef.current = tags
  tagModeRef.current = tagMode

  // 挂载即读最新一页（30s TTL 内不重建索引）；切 tab 重挂载也会刷新到最新
  useEffect(() => {
    void load(false)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 标签点击：事件委托（CSP 禁行内 handler，顶层容器统一拦截 .tl-tag）；已选标签再点 = 取消
  const toggleTag = (t: string) => {
    setTags((prev) => (prev.includes(t) ? prev.filter((x) => x !== t) : [...prev, t]))
  }
  const onTagClick = (e: Event) => {
    const target = e.target as HTMLElement | null
    const el = target?.closest?.('.tl-tag') as HTMLElement | null
    if (!el) return
    const t = el.dataset.tag ?? ''
    if (t) toggleTag(t)
  }

  // 回到最新（数据渲染完成后置底；双 setTimeout 保证 React 已提交 DOM）
  function scrollToBottom() {
    setTimeout(() => setTimeout(() => {
      const el = scrollRef.current
      if (el) el.scrollTop = el.scrollHeight
    }, 0), 0)
  }

  async function load(reload: boolean) {
    if (loadingRef.current) return
    loadingRef.current = true
    setLoading(true)
    try {
      const params = new URLSearchParams()
      const chat = selectedRef.current
      if (chat) params.set('chat', String(chat))
      if (tagsRef.current.length) {
        params.set('tag', tagsRef.current.join(','))
        params.set('tag_mode', tagModeRef.current)
      }
      if (reload) params.set('refresh', '1')
      if (!reload && cursorRef.current) {
        const [d, id] = cursorRef.current.split('.')
        params.set('before_date', d)
        params.set('before_msg_id', id)
      }
      const page = await api.timeline(params)
      setChats(page.chats)
      if (reload) {
        // 刷新：回到最新，列表重置为旧→新
        setItems(flip(page.items))
        setCursor(page.next_cursor || '')
        scrollToBottom()
      } else if (cursorRef.current) {
        // 加载更旧的一页：插到列表顶部，并补偿滚动位置避免跳动
        const prevH = scrollRef.current?.scrollHeight ?? 0
        setItems((prev) => [...flip(page.items), ...prev])
        setCursor(page.next_cursor || '')
        // 渲染完成后补偿滚动位置，避免插入旧页时视口跳动
        setTimeout(() => setTimeout(() => {
          const el = scrollRef.current
          if (el) el.scrollTop += el.scrollHeight - prevH
        }, 0), 0)
      } else {
        setItems(flip(page.items))
        setCursor(page.next_cursor || '')
        if (!selectedRef.current && page.chats.length > 0) {
          select(page.chats[0].chat_id, true, false)
          return
        }
        scrollToBottom()
      }
    } catch (e) {
      toast((e as Error).message)
    } finally {
      loadingRef.current = false
      setLoading(false)
    }
  }

  // 频道类型映射：/api/chats 的 TDLib 类型（channel/supergroup/basicGroup…）
  useEffect(() => {
    api.chats().then((list) => {
      const map: Record<number, string> = {}
      for (const c of list) map[c.id] = c.type || ''
      setChatTypes(map)
    }).catch(() => { /* 无类型信息时按频道样式渲染 */ })
  }, [])

  // 标签变化（增删/模式切换）：重载第一页
  useEffect(() => {
    if (selectedRef.current === 0 && tagsRef.current.length === 0) return
    setCursor('')
    cursorRef.current = ''
    setItems([])
    const params = new URLSearchParams()
    if (selectedRef.current) params.set('chat', String(selectedRef.current))
    if (tagsRef.current.length) {
      params.set('tag', tagsRef.current.join(','))
      params.set('tag_mode', tagModeRef.current)
    }
    api.timeline(params).then((page) => {
      setItems(flip(page.items))
      setCursor(page.next_cursor || '')
      setChats(page.chats)
      scrollToBottom()
    }).catch((e) => toast((e as Error).message))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tags, tagMode])

  function select(chatID: number, forceReload = true, refresh = forceReload) {
    setSelected(chatID)
    selectedRef.current = chatID
    setCursor('')
    cursorRef.current = ''
    setOpened(true)
    if (forceReload) {
      setItems([])
      const params = new URLSearchParams()
      if (chatID) params.set('chat', String(chatID))
      if (tagsRef.current.length) {
        params.set('tag', tagsRef.current.join(','))
        params.set('tag_mode', tagModeRef.current)
      }
      if (refresh) params.set('refresh', '1')
      api.timeline(params).then((page) => {
        setItems(flip(page.items))
        setCursor(page.next_cursor || '')
        setChats(page.chats)
        scrollToBottom()
      }).catch((e) => toast((e as Error).message))
    }
  }

  // 滚动到接近顶部时加载更旧的一页（最新在底部，上翻看旧）
  useEffect(() => {
    const el = scrollRef.current
    if (!el) return
    const onScroll = () => {
      if (el.scrollTop <= 300 && cursorRef.current && !loadingRef.current) {
        void load(false)
      }
    }
    el.addEventListener('scroll', onScroll)
    return () => el.removeEventListener('scroll', onScroll)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const grouped = groupByDay(items)
  const currentChat = chats.find((c) => c.chat_id === selected)
  const currentTitle = currentChat?.title || ''
  const isGroup = isGroupType(chatTypes[selected])
  const matching = items.length

  // 皮肤应用
  useEffect(() => {
    const r = document.documentElement.style
    if (skin.url.trim()) {
      r.setProperty('--tl-bg-img', `url("${skin.url.trim()}")`)
    } else {
      r.setProperty('--tl-bg-img', 'none')
    }
    r.setProperty('--tl-panel-alpha', String(skin.panelAlpha))
    r.setProperty('--tl-bubble-alpha', String(skin.bubbleAlpha))
    r.setProperty('--tl-blur', skin.blur + 'px')
    try { localStorage.setItem(SKIN_KEY, JSON.stringify(skin)) } catch { /* 忽略 */ }
  }, [skin])

  const showGlass = skin.panelAlpha < 1 || skin.blur > 0
  const showBubbleGlass = skin.bubbleAlpha < 1 || skin.blur > 0

  return (
    <div class={`tl${opened ? ' tl-open' : ''}`}>
      <div class={`tl-bg ${skin.url.trim() ? '' : skin.bg}`} />

      <aside class={`tl-list${showGlass ? ' glass' : ''}`}>
        <div class="tl-list-head">
          <span class="grow">频道</span>
          <button
            class="sm" disabled={refreshing} title="重新扫描 sidecar 元数据"
            onClick={() => {
              setRefreshing(true)
              void load(true).finally(() => setRefreshing(false))
            }}
          >
            {refreshing ? '…' : '↻'}
          </button>
        </div>
        <div class="tl-chats">
          {chats.length === 0 && <div class="tl-empty">暂无已下载消息<br />先下载一些内容再来</div>}
          {chats.map((c) => (
            <button
              key={c.chat_id}
              class={`tl-chat${selected === c.chat_id && opened ? ' on' : ''}`}
              onClick={() => select(c.chat_id)}
            >
              <span class="tl-avatar" style={`background:hsl(${avatarHue(c.chat_id)} 55% 42%)`}>
                {c.title.trim().slice(0, 1).toUpperCase() || '#'}
              </span>
              <span class="tl-chat-body">
                <span class="tl-chat-title">{c.title}</span>
                <span class="tl-chat-meta">
                  <span>{c.count} 条消息</span>
                  <span class="mono">{fmtSync(c.last_date)}</span>
                </span>
              </span>
            </button>
          ))}
        </div>
      </aside>

      <section class="tl-main">
        <div class={`tl-main-head${showGlass ? ' glass' : ''}`}>
          <button class="sm tl-back" onClick={() => setOpened(false)}>← 频道</button>
          <span class="tl-main-title">{currentTitle}</span>
          <span class="tl-type">{isGroup ? '群组' : '频道'}</span>
          <span class="mono tl-main-sub">{tags.length > 0 ? `筛选 ${matching} 条` : `${items.length} 条`}</span>
          <span class="grow" />
          <SkinPanel
            skin={skin}
            onApply={(s) => {
              setSkin(s)
              toast('外观已应用')
            }}
          />
        </div>

        {tags.length > 0 && (
          <div class="tl-tagbar show">
            <span class="tl-tagbar-label">筛选标签</span>
            {tags.map((t) => (
              <span class="tl-tag-pill" key={t}>
                #{t}
                <button class="tl-tag-x" title="移除该标签" onClick={() => toggleTag(t)}>✕</button>
              </span>
            ))}
            <span class="tl-tag-mode">
              <button class={tagMode === 'or' ? 'on' : ''} title="含任一标签即命中" onClick={() => setTagMode('or')}>并集</button>
              <button class={tagMode === 'and' ? 'on' : ''} title="须同时含全部标签" onClick={() => setTagMode('and')}>交集</button>
            </span>
            <button class="sm" onClick={() => setTags([])}>✕ 清除全部</button>
          </div>
        )}

        <div class="tl-scroll" ref={scrollRef} onClick={onTagClick}>
          {loading && <div class="tl-loading">加载中…</div>}
          {!cursor && !loading && items.length > 0 && <div class="tl-loading">— 没有更早的消息 —</div>}
          {grouped.map((g) => (
            <div class="tl-day">
              <div class="tl-day-head"><span>{fmtDay(g.firstTs)}</span></div>
              {g.groups.map((album, gi) => {
                const first = album[0]
                const rowKey = `${selected}-${gi}-${first.message_id}`
                // 群组：头像 + 发送者名（连续消息压缩头像由 groupByDay 不完全支持，简单处理）
                const body = (
                  <>
                    {album.length === 1 ? (
                      <MediaItem entry={album[0]} />
                    ) : (
                      <div class="tl-album">
                        {album.map((e) => <MediaItem key={e.rel_path} entry={e} />)}
                      </div>
                    )}
                    {(first.caption || first.message_url) && (
                      <div class="tl-caption-wrap">
                        {first.caption && (
                          <p class="tl-caption" dangerouslySetInnerHTML={{ __html: tagify(first.caption) }} />
                        )}
                        {first.message_url && (
                          <a class="tl-link" href={first.message_url} target="_blank" rel="noopener noreferrer">
                            查看原消息 ↗
                          </a>
                        )}
                      </div>
                    )}
                    <div class="tl-msg-meta">
                      <span class="mono">{fmtClock(first.date)}</span>
                      {first.sender_id && isGroup && <span class="mono">ID {first.sender_id}</span>}
                    </div>
                  </>
                )
                if (isGroup) {
                  return (
                    <div class="tl-msg-row" key={rowKey}>
                      <span class="tl-msg-avatar" style={`background:hsl(${avatarHue(first.sender_id || first.chat_id)} 55% 45%)`}>
                        {String(first.sender_id || '?').replace('-', '').slice(0, 1).toUpperCase()}
                      </span>
                      <div class="tl-msg-body">
                        <span class="tl-sender">{first.sender_id ? `ID ${first.sender_id}` : ''}</span>
                        <div class={`tl-bubble${showBubbleGlass ? ' glass' : ''}`}>{body}</div>
                      </div>
                    </div>
                  )
                }
                // 频道：靠左气泡，无头像无名字
                return (
                  <div class="tl-chan-row" key={rowKey}>
                    <div class="tl-msg-body">
                      <div class={`tl-bubble${showBubbleGlass ? ' glass' : ''}`}>{body}</div>
                    </div>
                  </div>
                )
              })}
            </div>
          ))}
          {!loading && items.length === 0 && (
            <div class="tl-empty">
              {tags.length > 0 ? `没有含 ${tags.map((t) => '#' + t).join(tagMode === 'and' ? ' + ' : ' / ')} 的消息` : '暂无消息'}
            </div>
          )}
        </div>
      </section>
    </div>
  )
}

export type { TimelineChat, TimelineEntry, TimelinePage }