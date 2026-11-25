import axios, { AxiosInstance } from 'axios'

export interface ManagerAuthStatus {
  mode: 'form' | 'basic' | 'proxy' | 'none';
  authenticated: boolean;
  username?: string;
  login_url: string;
  logout_url: string;
}

// The manager has no client-side routes; both its directory and index.html are entry points.
const managerPath = location.pathname.replace(/\/index\.html$/, '').replace(/\/+$/, '')
export const managerBasePath = location.origin + managerPath

let authStatus: ManagerAuthStatus | null = null
let redirecting = false

export function safeReturnPath(candidate: string): string {
  const fallback = managerPath + '/'
  // eslint-disable-next-line no-control-regex -- Reject controls before URL normalization.
  if (!candidate.startsWith('/') || candidate.startsWith('//') || /[\\\s\u0000-\u001f\u007f]/.test(candidate)) return fallback

  const url = new URL(candidate, location.origin)
  // Only return to actual manager UI entry points, never auth, API or room/CDP URLs (also at /).
  if (url.origin !== location.origin || ![managerPath || '/', fallback, managerPath + '/index.html'].includes(url.pathname)) return fallback
  return url.pathname + url.search + url.hash
}

function redirectToLogin(): void {
  if (redirecting || authStatus?.mode !== 'form') return
  redirecting = true
  const next = safeReturnPath(location.pathname + location.search + location.hash)
  location.replace(authStatus.login_url + '?next=' + encodeURIComponent(next))
}

function parseAuthStatus(data: unknown): ManagerAuthStatus {
  const status = data as Partial<ManagerAuthStatus> | null
  if (!status || !['form', 'basic', 'proxy', 'none'].includes(status.mode || '') ||
      typeof status.authenticated !== 'boolean' ||
      (status.username !== undefined && typeof status.username !== 'string') ||
      status.login_url !== managerPath + '/auth/login' ||
      status.logout_url !== managerPath + '/auth/logout') {
    throw new Error('Invalid Manager authentication status response.')
  }
  return {
    mode: status.mode as ManagerAuthStatus['mode'],
    authenticated: status.authenticated,
    username: status.username,
    login_url: status.login_url,
    logout_url: status.logout_url,
  }
}

// A null result means full-page navigation is in progress: do not mount Vue or start API/SSE calls.
export async function initializeManagerAuth(): Promise<ManagerAuthStatus | null> {
  const response = await axios.get<unknown>(managerBasePath + '/auth/status', { timeout: 15000 })
  authStatus = parseAuthStatus(response.data)
  if (authStatus.mode === 'form' && !authStatus.authenticated) {
    redirectToLogin()
    return null
  }
  return authStatus
}

export function installManagerAuthInterceptor(client: AxiosInstance): void {
  client.interceptors.response.use(response => response, (error: unknown) => {
    if (authStatus?.mode === 'form' && axios.isAxiosError(error) && error.response?.status === 401 && error.config) {
      const url = new URL(client.getUri(error.config), location.href)
      const apiPath = managerPath + '/api'
      if (url.origin === location.origin && (url.pathname === apiPath || url.pathname.startsWith(apiPath + '/'))) {
        redirectToLogin()
      }
    }
    return Promise.reject(error)
  })
}

// Generated APIs use default Axios. axios.create() instances must install this separately.
installManagerAuthInterceptor(axios)
