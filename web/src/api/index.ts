import { api } from './client'

export interface ApiResponse<T> {
  code: number
  msg: string
  data: T
}

// 认证模块
export interface LoginResponse {
  accessToken: string
  username: string
  expiresAt: number
}

export const authApi = {
  login: async (params: { username: string; password: string }) => {
    const res = await api.post<ApiResponse<LoginResponse>>('/auth/login', params)
    return res.data.data
  },
  logout: async () => {
    const res = await api.post<ApiResponse<void>>('/auth/logout')
    return res.data
  },
}

/**
 * 仪表盘统计。
 * 字段名与 Go `orderStatisticsAPIHandler` 完全一致：
 * todayOrder / todaySuccessOrder / todayCloseOrder / todayMoney / countOrder / countMoney。
 * 后端不提供按渠道拆分的金额，因此这里也不声明渠道字段，避免前端展示恒为 0 的伪数据。
 */
export interface DashboardStats {
  todayOrder: number
  todaySuccessOrder: number
  todayCloseOrder: number
  todayMoney: string
  countOrder: number
  countMoney: string
}

export interface SystemConfig {
  phpOs: string
  server: string
  phpVersion: string
  mysqlVersion: string
  thinkphpVersion: string
  gdInfo: string
  appVersion: string
  runTime: string
}

export const dashboardApi = {
  getStats: async () => {
    const res = await api.get<ApiResponse<DashboardStats>>('/config/status')
    return res.data.data
  },
  getSystemConfig: async () => {
    const res = await api.get<ApiResponse<SystemConfig>>('/config/get')
    return res.data.data
  },
}

// 订单管理模块（管理端列表使用后端 snake_case 字段）
export interface OrderItem {
  id: number
  order_id: string
  pay_id: string
  type: number
  type_text: string
  price: string
  really_price: string
  state: number
  state_text: string
  create_time: string
  create_date: number
  pay_date: number
  close_date: number
  param?: string
  /** 所属商户 ID；0 = 系统商户（多商户改造前的存量原生单）。 */
  merchant_id: number
  /** 0=VMQ 原生 1=EPay V1 2=EPay V2 */
  protocol: number
}

export interface OrderListResponse {
  items: OrderItem[]
  total: number
}

export const orderApi = {
  list: async (params: { page: number; limit: number; state?: number; merchantId?: number }) => {
    const { merchantId, ...rest } = params
    const res = await api.get<ApiResponse<OrderListResponse>>('/order/list', {
      // 后端按 snake_case 读 query；merchantId 为空时不传，避免被当成「筛选系统商户 0」
      params: merchantId === undefined ? rest : { ...rest, merchant_id: merchantId },
    })
    return res.data.data
  },
  reissue: async (id: number) => {
    const res = await api.post<ApiResponse<void>>(`/order/reissue/${id}`)
    return res.data
  },
  delete: async (id: number) => {
    const res = await api.delete<ApiResponse<void>>(`/order/${id}`)
    return res.data
  },
  closeExpired: async () => {
    const res = await api.post<ApiResponse<{ count: number }>>('/order/expired')
    return res.data.data
  },
  deleteExpired: async () => {
    const res = await api.delete<ApiResponse<{ count: number }>>('/order/expired')
    return res.data.data
  },
  deleteLast: async () => {
    // 后端 /order/last 固定清理 24 小时前的订单，不接受 days 参数
    const res = await api.delete<ApiResponse<{ count: number }>>('/order/last')
    return res.data.data
  },
}

// 二维码管理模块
export interface QrcodeItem {
  id: number
  pay_url: string
  price: string
  type: number
  type_text: string
  state: number
  state_text: string
}

export interface QrcodeListResponse {
  items: QrcodeItem[]
  total: number
}

export const qrcodeApi = {
  list: async (params: { type: 'wechat' | 'alipay'; page: number; limit: number }) => {
    const url = params.type === 'wechat' ? '/qrcode/wechat' : '/qrcode/alipay'
    const res = await api.get<ApiResponse<QrcodeListResponse>>(url, {
      params: { page: params.page, limit: params.limit },
    })
    return res.data.data
  },
  create: async (data: { type: number; payUrl: string; price: string }) => {
    const url = data.type === 1 ? '/qrcode/wechat' : '/qrcode/alipay'
    // 后端 createQRCodeHandler 读取 pay_url，此处必须使用同名字段
    const res = await api.post<ApiResponse<void>>(url, {
      pay_url: data.payUrl,
      price: data.price,
    })
    return res.data
  },
  setState: async (id: number, state: number) => {
    const res = await api.post<ApiResponse<void>>(`/qrcode/bind/${id}`, { state })
    return res.data
  },
  delete: async (type: 'wechat' | 'alipay', id: number) => {
    const url = type === 'wechat' ? `/qrcode/wechat/${id}` : `/qrcode/alipay/${id}`
    const res = await api.delete<ApiResponse<void>>(url)
    return res.data
  },
}

// 监控端模块
// jkstate 是监控端在线状态（-1=未绑定、0=已掉线、1=运行正常），由后端心跳与生命周期任务维护，并非总开关。
export interface MonitorSettings {
  jkstate: string
  lastheart: string
  lastpay: string
}

export const monitorApi = {
  get: async () => {
    const res = await api.get<ApiResponse<MonitorSettings>>('/config/monitor')
    return res.data.data
  },
  // 手动覆盖监控状态，仅为兼容旧客户端保留；界面状态展示一律以 get 返回的 jkstate 为准。
  update: async (data: { jkstate: string }) => {
    const res = await api.post<ApiResponse<void>>('/config/monitor', data)
    return res.data
  },
}

// 系统设置模块
export interface SystemSettings {
  user: string
  pass?: string
  notifyUrl: string
  returnUrl: string
  key: string
  close: string
  payQf: string
  wxpay: string
  zfbpay: string
  /** 收款方支付宝账号，用于生成转账确认单；留空即退回二维码扫码。 */
  alipayAccount: string
}

export const settingsApi = {
  get: async () => {
    const res = await api.get<ApiResponse<SystemSettings>>('/config/settings')
    return res.data.data
  },
  update: async (data: Partial<SystemSettings>) => {
    const res = await api.post<ApiResponse<void>>('/config/settings', data)
    return res.data
  },
}

/**
 * 公开收银台订单视图。
 * 与 Go `PublicOrderView` 严格一一对应：不含 orderId、param、notifyUrl、returnUrl 与独立签名字段。
 * 回跳地址必须在支付成功后单独调用 getReturnUrl 获取。
 */
export interface PublicOrderInfo {
  payId: string
  payType: number
  price: string
  reallyPrice: string
  payUrl: string
  isAuto: number
  state: number
  stateText: string
  timeOut: number
  date: number
  remainingSeconds: number
  /**
   * 支付宝转账确认单深度链接（alipays://）。
   * 为空表示未配置收款账号或非支付宝订单，此时退回二维码扫码。
   */
  alipayScheme?: string
}

export interface CreateOrderResult {
  payId: string
  orderId: string
  publicToken: string
  payType: number
  price: string
  reallyPrice: string
  payUrl: string
  isAuto: number
  redirectUrl: string
}

// 商户管理模块（EPay 多商户）
//
// 契约以 internal/http/handler/merchant.go 的 apiMerchantViewData 为准，字段是 camelCase。
// 三条硬约束必须在 UI 上体现，否则运营会以为「密钥丢了」：
//   1. 明文密钥只在 create / rotateKey 的响应里出现一次，之后任何接口都拿不到；
//   2. update 的 enabled 是必填 bool（后端 requiredBoolParam），不能省略；
//   3. 轮换后旧密钥仍可验签（按代次保留），但商户端要尽快换成新密钥。
export interface MerchantItem {
  id: number
  merchantNo: string
  name: string
  enabled: boolean
  closeMinutes: number
  note: string
  v1Enabled: boolean
  v2Enabled: boolean
  /** 末 4 位，用于运营比对「商户配的是哪把」；不是密钥本身 */
  v1Hint: string
  v2Hint: string
  createdAt: string
  updatedAt: string
}

export interface MerchantCreateResult extends MerchantItem {
  /** 明文共享密钥，仅此一次 */
  secret: string
  hint: string
  publicKey?: string
}

export interface MerchantRotateResult {
  protocol: number
  hint: string
  /** V1 轮换才有明文共享密钥 */
  secret?: string
  /** V2 轮换才有平台公钥 */
  publicKey?: string
}

export const merchantApi = {
  list: async () => {
    const res = await api.get<ApiResponse<{ total: number; items: MerchantItem[] }>>('/merchant/list')
    return res.data.data
  },
  create: async (data: { name: string; closeMinutes?: number; note?: string }) => {
    const res = await api.post<ApiResponse<MerchantCreateResult>>('/merchant/create', data)
    return res.data.data
  },
  update: async (
    id: number,
    data: { name: string; enabled: boolean; closeMinutes?: number; note?: string }
  ) => {
    const res = await api.post<ApiResponse<MerchantItem>>(`/merchant/update/${id}`, data)
    return res.data
  },
  rotateKey: async (id: number, protocol: 1 | 2) => {
    const res = await api.post<ApiResponse<MerchantRotateResult>>(`/merchant/key/rotate/${id}`, {
      protocol,
    })
    return res.data.data
  },
  publicKey: async (id: number) => {
    const res = await api.get<ApiResponse<{ publicKey: string }>>(`/merchant/key/public/${id}`)
    return res.data.data
  },
}

export const publicPaymentApi = {
  /** 公开令牌是 bearer 凭据，必须走路径参数，与 Go 路由 /order/get/:id 对齐 */
  getOrder: async (publicToken: string) => {
    const res = await api.get<ApiResponse<PublicOrderInfo>>(
      `/order/get/${encodeURIComponent(publicToken)}`
    )
    return res.data.data
  },
  checkOrder: async (publicToken: string) => {
    const res = await api.get<ApiResponse<{ state: number; remainingSeconds: number }>>(
      `/order/check/${encodeURIComponent(publicToken)}`
    )
    return res.data.data
  },
  /**
   * 获取服务端生成并签名的商户回跳地址。
   * 后端仅在订单已支付时才返回，未支付会给出明确业务错误。
   */
  getReturnUrl: async (publicToken: string) => {
    const res = await api.get<ApiResponse<{ returnUrl: string; mode: string }>>(
      `/order/return-url/${encodeURIComponent(publicToken)}`
    )
    return res.data.data
  },
  createTestOrder: async (data: {
    payId: string
    type: number
    price: string
    sign: string
    param?: string
    notifyUrl?: string
    returnUrl?: string
  }) => {
    const res = await api.post<ApiResponse<CreateOrderResult>>('/order/create', data)
    return res.data.data
  },
}
