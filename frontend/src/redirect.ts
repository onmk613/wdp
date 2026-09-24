// 登录后回跳目标的校验（纯函数，无路由依赖，可单测）。
// 仅接受站内路径：单个 / 开头且非协议相对 //evil.com 形态——后者会被
// 浏览器当跨源跳转，是经典开放重定向绕过面。非法值回 /hosts。
export function safeRedirect(r: unknown): string {
  if (typeof r === 'string' && r.startsWith('/') && !r.startsWith('//')) return r
  return '/hosts'
}
