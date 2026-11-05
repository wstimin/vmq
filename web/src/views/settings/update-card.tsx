import React, { useEffect, useRef, useState } from 'react'
import { api } from '@/api/client'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Download, RefreshCw } from 'lucide-react'
import { toast } from 'sonner'

interface UpdateState {
  phase: string
  progress: number
  message: string
  current_version: string
  latest_version: string
  repository: string
  release_url?: string
  available: boolean
  has_token: boolean
  managed: boolean
}

const busyPhases = ['downloading', 'verifying', 'installing', 'restarting']

export const UpdateCard: React.FC = () => {
  const [state, setState] = useState<UpdateState | null>(null)
  const [repository, setRepository] = useState('wstimin/vmq')
  const [token, setToken] = useState('')
  const [working, setWorking] = useState(false)
  const [waiting, setWaiting] = useState(false)
  const [connectionMessage, setConnectionMessage] = useState('')
  const mounted = useRef(true)
  const expectedVersion = useRef('')
  const reloadTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const checked = useRef(false)
  const busy = !!state && busyPhases.includes(state.phase)

  useEffect(() => {
    mounted.current = true
    let timer: ReturnType<typeof setTimeout> | null = null
    let failures = 0
    const poll = async () => {
      try {
        const response = await api.get('/system/update/status', { timeout: 5000 })
        if (!mounted.current) return
        const next = response.data.data as UpdateState
        setState(next)
        setConnectionMessage('')
        if (!expectedVersion.current) setRepository(next.repository)
        if (busyPhases.includes(next.phase)) {
          expectedVersion.current = next.latest_version
          setWaiting(true)
        }
        if (next.phase === 'completed' && expectedVersion.current && next.current_version === expectedVersion.current) {
          setWaiting(false)
          reloadTimer.current = setTimeout(() => window.location.reload(), 1200)
          return
        }
        if (next.phase === 'failed' || next.phase === 'rolled_back') {
          setWaiting(false)
          expectedVersion.current = ''
        }
        if (!checked.current && !busyPhases.includes(next.phase) && !['failed', 'rolled_back'].includes(next.phase) && !expectedVersion.current) {
          checked.current = true
          void api.post('/system/update/check', {}, { timeout: 30000 }).then(response => {
            if (mounted.current) setState(response.data.data as UpdateState)
          }).catch(error => {
            if (mounted.current) setConnectionMessage(error instanceof Error ? error.message : '检测更新失败')
          })
        }
        failures = 0
      } catch {
        if (!mounted.current) return
        failures++
        if (expectedVersion.current) setConnectionMessage('服务正在重启，等待连接恢复…')
        else setConnectionMessage('无法读取更新状态，请检查服务连接。')
        if (failures >= 60) {
          setWaiting(false)
          setConnectionMessage('等待连接超过两分钟，请检查服务日志；恢复后可手动刷新。')
          return
        }
      }
      if (mounted.current) timer = setTimeout(poll, expectedVersion.current ? 1500 : 10000)
    }
    void poll()
    return () => {
      mounted.current = false
      if (timer) clearTimeout(timer)
      if (reloadTimer.current) clearTimeout(reloadTimer.current)
    }
  }, [])

  const perform = async (action: 'source' | 'check' | 'start') => {
    setWorking(true)
    try {
      const response = await api.post(`/system/update/${action}`, action === 'source' ? { repository, token } : {}, { timeout: 30000 })
      const next = response.data.data as UpdateState
      setState(next)
      if (action === 'source') { setToken(''); toast.success('更新源已保存') }
      if (action === 'check' && !next.available) toast.success(next.message)
      if (action === 'start') { expectedVersion.current = next.latest_version; setWaiting(true) }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '操作失败')
    } finally { setWorking(false) }
  }

  return (
    <Card className="p-6">
      <CardHeader className="p-0 pb-5">
        <CardTitle className="text-base flex items-center gap-2"><Download className="size-4 text-primary" />版本更新</CardTitle>
        <p className="text-xs text-muted-foreground mt-2">当前版本：{state?.current_version || '读取中'}{state?.latest_version ? `　最新版本：${state.latest_version}` : ''}</p>
      </CardHeader>
      <CardContent className="p-0 space-y-4">
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          <label className="text-xs text-muted-foreground space-y-2"><span>GitHub 更新仓库</span><Input value={repository} disabled={busy || waiting} onChange={e => setRepository(e.target.value)} placeholder="owner/repository" /></label>
          <label className="text-xs text-muted-foreground space-y-2"><span>GitHub 访问令牌（公开仓库可留空）</span><Input type="password" autoComplete="new-password" value={token} disabled={busy || waiting} onChange={e => setToken(e.target.value)} placeholder={state?.has_token ? '已保存，留空保持不变' : '私有仓库需要具有读取 Release 权限的令牌'} /></label>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" disabled={working || busy || waiting} onClick={() => void perform('source')}>保存更新源</Button>
          <Button variant="outline" disabled={working || busy || waiting} onClick={() => void perform('check')}><RefreshCw className={`size-3.5 ${working ? 'animate-spin' : ''}`} />检测新版本</Button>
          <Button disabled={working || busy || waiting || !state?.available || !state?.managed} onClick={() => void perform('start')}><Download className="size-3.5" />立即更新</Button>
        </div>
        {(busy || waiting || state?.phase === 'completed') && <div className="space-y-2" aria-live="polite"><progress aria-label="版本更新进度" max={100} value={state?.progress || 0} className="h-2.5 w-full accent-primary" /><p className="text-xs text-muted-foreground">{state?.progress || 0}%　{connectionMessage || state?.message}</p></div>}
        {!busy && !waiting && <p role="status" className={`text-xs ${state?.phase === 'failed' || state?.phase === 'rolled_back' ? 'text-destructive' : 'text-muted-foreground'}`}>{connectionMessage || state?.message}</p>}
        {!state?.managed && <p className="text-xs text-muted-foreground">请先使用安装包的 start.sh 配置开机服务，即可启用自动重启和在线更新。</p>}
        <p className="text-xs text-muted-foreground">从你自己的改造版 Release 更新，自动校验文件并保留原程序；配置、订单和收款码保持原样。新程序启动失败会自动恢复。{state?.release_url && <a href={state.release_url} target="_blank" rel="noreferrer" className="text-primary ml-1">查看 Release</a>}</p>
      </CardContent>
    </Card>
  )
}
