import React, { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { merchantApi, MerchantItem, MerchantRotateResult } from '@/api'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Table, TableHeader, TableBody, TableHead, TableRow, TableCell } from '@/components/ui/table'
import { Switch } from '@/components/ui/switch'
import { CopyButton } from '@/components/common/copy-button'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Plus, Store, KeyRound, Pencil, ShieldCheck, AlertTriangle } from 'lucide-react'
import { toast } from 'sonner'

/**
 * 一次性密钥弹窗的载荷。
 * 明文只存在于 create / rotateKey 的响应里，关掉这个弹窗就再也拿不到——
 * 所以这里必须把「商户号 + 密钥 + 配置到哪」一次讲清，且不允许误关后重查。
 */
interface SecretPayload {
  title: string
  merchantNo: string
  protocol: number
  secret?: string
  publicKey?: string
  hint?: string
}

const protocolLabel = (protocol: number) => {
  if (protocol === 1) return 'EPay V1'
  if (protocol === 2) return 'EPay V2'
  return 'VMQ 原生'
}

export const MerchantsView: React.FC = () => {
  const queryClient = useQueryClient()

  const { data, isLoading } = useQuery({
    queryKey: ['merchants'],
    queryFn: () => merchantApi.list(),
  })

  // 创建
  const [createOpen, setCreateOpen] = useState(false)
  const [createName, setCreateName] = useState('')
  const [createClose, setCreateClose] = useState('30')
  const [createNote, setCreateNote] = useState('')

  // 编辑
  const [editTarget, setEditTarget] = useState<MerchantItem | null>(null)
  const [editName, setEditName] = useState('')
  const [editClose, setEditClose] = useState('30')
  const [editNote, setEditNote] = useState('')

  // 轮换
  const [rotateTarget, setRotateTarget] = useState<MerchantItem | null>(null)

  // 一次性密钥 / 公钥展示
  const [secret, setSecret] = useState<SecretPayload | null>(null)

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['merchants'] })

  const createMutation = useMutation({
    mutationFn: () =>
      merchantApi.create({
        name: createName.trim(),
        closeMinutes: Number(createClose) || 0,
        note: createNote.trim(),
      }),
    onSuccess: (result) => {
      toast.success(`商户 ${result.merchantNo} 已创建`)
      setCreateOpen(false)
      setCreateName('')
      setCreateClose('30')
      setCreateNote('')
      invalidate()
      setSecret({
        title: '商户已创建 — 请立即保存密钥',
        merchantNo: result.merchantNo,
        protocol: 1,
        secret: result.secret,
        publicKey: result.publicKey,
        hint: result.hint,
      })
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : '创建失败'),
  })

  const updateMutation = useMutation({
    mutationFn: (params: { item: MerchantItem; enabled?: boolean }) =>
      merchantApi.update(params.item.id, {
        name: editName.trim() || params.item.name,
        enabled: params.enabled ?? params.item.enabled,
        closeMinutes: Number(editClose) || 0,
        note: editNote.trim(),
      }),
    onSuccess: () => {
      toast.success('商户已更新')
      setEditTarget(null)
      invalidate()
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : '更新失败'),
  })

  const toggleEnabledMutation = useMutation({
    mutationFn: (item: MerchantItem) =>
      merchantApi.update(item.id, {
        name: item.name,
        enabled: !item.enabled,
        closeMinutes: item.closeMinutes,
        note: item.note,
      }),
    onSuccess: () => {
      toast.success('状态已更新')
      invalidate()
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : '状态切换失败'),
  })

  const rotateMutation = useMutation({
    mutationFn: (params: { id: number; protocol: 1 | 2 }) =>
      merchantApi.rotateKey(params.id, params.protocol),
    onSuccess: (result: MerchantRotateResult, params) => {
      const target = rotateTarget
      toast.success('密钥已轮换')
      setRotateTarget(null)
      invalidate()
      setSecret({
        title: '密钥已轮换 — 请配置到商户端',
        merchantNo: target?.merchantNo ?? '',
        protocol: result.protocol ?? params.protocol,
        secret: result.secret,
        publicKey: result.publicKey,
        hint: result.hint,
      })
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : '轮换失败'),
  })

  const openEdit = (item: MerchantItem) => {
    setEditTarget(item)
    setEditName(item.name)
    setEditClose(String(item.closeMinutes))
    setEditNote(item.note)
  }

  const openRotate = (item: MerchantItem) => {
    setRotateTarget(item)
  }

  return (
    <div className="space-y-6">
      {/* 顶部操作区 */}
      <div className="flex items-center justify-between bg-card/60 border border-border/60 p-4 rounded-3xl backdrop-blur-xs">
        <div className="flex items-center gap-2">
          <div className="p-2 rounded-2xl bg-indigo-500/10 text-indigo-600">
            <Store className="size-4" />
          </div>
          <div>
            <div className="text-sm font-semibold">EPay 商户</div>
            <div className="text-xs text-muted-foreground">
              共 {data?.total ?? 0} 个商户 · 密钥明文只在创建/轮换时展示一次
            </div>
          </div>
        </div>

        <Button size="sm" className="gap-1.5 rounded-2xl" onClick={() => setCreateOpen(true)}>
          <Plus className="size-3.5" />
          新建商户
        </Button>
      </div>

      {/* 商户列表 */}
      <Card className="p-6">
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>商户号</TableHead>
                <TableHead>名称 / 备注</TableHead>
                <TableHead>有效期</TableHead>
                <TableHead>密钥</TableHead>
                <TableHead>状态</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {isLoading ? (
                <TableRow>
                  <TableCell colSpan={6} className="text-center py-10 text-muted-foreground text-xs">
                    数据加载中...
                  </TableCell>
                </TableRow>
              ) : data?.items && data.items.length > 0 ? (
                data.items.map((item) => (
                  <TableRow key={item.id}>
                    <TableCell>
                      <div className="flex items-center gap-1">
                        <span className="font-mono text-xs font-semibold">{item.merchantNo}</span>
                        <CopyButton text={item.merchantNo} label="商户号" />
                      </div>
                    </TableCell>
                    <TableCell>
                      <div className="text-xs font-medium">{item.name}</div>
                      {item.note && (
                        <div className="text-[11px] text-muted-foreground max-w-[180px] truncate">
                          {item.note}
                        </div>
                      )}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground whitespace-nowrap">
                      {item.closeMinutes > 0 ? `${item.closeMinutes} 分钟` : '跟随全局'}
                    </TableCell>
                    <TableCell>
                      <div className="flex flex-col gap-1">
                        {item.v1Enabled ? (
                          <span className="inline-flex items-center gap-1 text-[11px] text-emerald-600">
                            <ShieldCheck className="size-3" />
                            V1 ···{item.v1Hint || '----'}
                          </span>
                        ) : (
                          <span className="text-[11px] text-muted-foreground">V1 未启用</span>
                        )}
                        {item.v2Enabled && (
                          <span className="text-[11px] text-blue-600">V2 ···{item.v2Hint || '----'}</span>
                        )}
                      </div>
                    </TableCell>
                    <TableCell>
                      <Switch
                        checked={item.enabled}
                        onCheckedChange={() => toggleEnabledMutation.mutate(item)}
                        disabled={toggleEnabledMutation.isPending}
                      />
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex items-center justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="sm"
                          className="h-8 px-2 text-xs"
                          onClick={() => openEdit(item)}
                        >
                          <Pencil className="size-3.5" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          className="h-8 px-2 text-xs"
                          onClick={() => openRotate(item)}
                        >
                          <KeyRound className="size-3.5" />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))
              ) : (
                <TableRow>
                  <TableCell colSpan={6} className="text-center py-12 text-muted-foreground text-xs">
                    还没有商户。点击右上角「新建商户」创建第一个 EPay 接入方。
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {/* 创建商户 */}
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>新建 EPay 商户</DialogTitle>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">商户名称 *</label>
              <Input
                placeholder="例如：独角数卡"
                value={createName}
                onChange={(e) => setCreateName(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                订单有效期（分钟，0 = 跟随系统全局）
              </label>
              <Input
                type="number"
                min={0}
                value={createClose}
                onChange={(e) => setCreateClose(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">备注</label>
              <Input
                placeholder="选填，便于运营区分"
                value={createNote}
                onChange={(e) => setCreateNote(e.target.value)}
              />
            </div>
            <p className="text-[11px] text-muted-foreground leading-relaxed">
              创建后会自动下发一把 V1 共享密钥，<strong>明文只在下一步显示一次</strong>，
              请当场复制到商户后台。
            </p>
            <div className="flex justify-end gap-2 pt-2">
              <Button variant="outline" onClick={() => setCreateOpen(false)}>
                取消
              </Button>
              <Button
                onClick={() => createMutation.mutate()}
                disabled={!createName.trim() || createMutation.isPending}
              >
                创建并生成密钥
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>

      {/* 编辑商户 */}
      <Dialog open={!!editTarget} onOpenChange={(open) => !open && setEditTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>编辑商户 {editTarget?.merchantNo}</DialogTitle>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">商户名称</label>
              <Input value={editName} onChange={(e) => setEditName(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                订单有效期（分钟，0 = 跟随全局）
              </label>
              <Input
                type="number"
                min={0}
                value={editClose}
                onChange={(e) => setEditClose(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">备注</label>
              <Input value={editNote} onChange={(e) => setEditNote(e.target.value)} />
            </div>
            <div className="flex justify-end gap-2 pt-2">
              <Button variant="outline" onClick={() => setEditTarget(null)}>
                取消
              </Button>
              <Button
                onClick={() =>
                  editTarget && updateMutation.mutate({ item: editTarget, enabled: editTarget.enabled })
                }
                disabled={updateMutation.isPending}
              >
                保存
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>

      {/* 轮换密钥 */}
      <Dialog open={!!rotateTarget} onOpenChange={(open) => !open && setRotateTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>轮换密钥 · {rotateTarget?.merchantNo}</DialogTitle>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="flex items-start gap-2 rounded-2xl bg-amber-500/10 border border-amber-500/20 p-3">
              <AlertTriangle className="size-4 text-amber-600 shrink-0 mt-0.5" />
              <p className="text-[11px] text-amber-700 leading-relaxed">
                轮换会立即生成新密钥。旧密钥仍可验证历史请求（按代次保留），
                但商户端要尽快替换，否则新发起的请求会签名失败。
              </p>
            </div>
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">协议版本</label>
              {/* V2（RSA 双向签名）尚未开放：后端没有 peer_public_key 写入入口，
                  对未开通商户轮换必然失败。这里只保留 V1，不给运营一个点了必报错的入口。 */}
              <div className="h-9 rounded-xl border border-border bg-muted/40 px-3 flex items-center text-xs text-muted-foreground">
                EPay V1（共享密钥，MD5）
              </div>
            </div>
            <div className="flex justify-end gap-2 pt-2">
              <Button variant="outline" onClick={() => setRotateTarget(null)}>
                取消
              </Button>
              <Button
                variant="destructive"
                onClick={() =>
                  rotateTarget &&
                  rotateMutation.mutate({ id: rotateTarget.id, protocol: 1 })
                }
                disabled={rotateMutation.isPending}
              >
                确认轮换
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>

      {/* 一次性密钥 / 公钥展示 */}
      <Dialog open={!!secret} onOpenChange={(open) => !open && setSecret(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{secret?.title}</DialogTitle>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="text-xs text-muted-foreground">
              商户号 <span className="font-mono text-foreground">{secret?.merchantNo}</span>
              <span className="mx-2">·</span>
              协议 <span className="text-foreground">{protocolLabel(secret?.protocol ?? 0)}</span>
            </div>

            {secret?.secret && (
              <div className="space-y-1.5">
                <label className="text-xs font-medium text-muted-foreground">
                  共享密钥（商户后台的「商户密钥」）
                </label>
                <div className="flex items-center gap-2">
                  <Input readOnly value={secret.secret} className="font-mono text-xs" />
                  <CopyButton text={secret.secret} label="密钥" />
                </div>
              </div>
            )}

            {secret?.publicKey && (
              <div className="space-y-1.5">
                <label className="text-xs font-medium text-muted-foreground">平台公钥（PEM）</label>
                <textarea
                  readOnly
                  value={secret.publicKey}
                  rows={8}
                  className="w-full rounded-xl border border-border bg-muted/40 p-3 font-mono text-[10px] leading-relaxed"
                />
              </div>
            )}

            <div className="flex items-start gap-2 rounded-2xl bg-destructive/10 border border-destructive/20 p-3">
              <AlertTriangle className="size-4 text-destructive shrink-0 mt-0.5" />
              <p className="text-[11px] text-destructive leading-relaxed">
                关闭后将<strong>无法再次查看</strong>明文。丢失只能重新轮换，
                而轮换会让商户端旧配置立刻失效。
              </p>
            </div>

            <div className="flex justify-end">
              <Button onClick={() => setSecret(null)}>我已保存，关闭</Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}
