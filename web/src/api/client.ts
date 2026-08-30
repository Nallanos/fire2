// Thin fetch wrapper around the fire2 orchestrator API. No framework beyond
// what's needed: the app is a client of an existing Go API, not a second
// backend — see internal/packages/orchestrator/http.go and auth_http.go for
// the routes this mirrors.

const API_URL = import.meta.env.VITE_API_URL as string

export interface User {
  id: string
  email: string
}

export interface AuthResponse {
  token: string
  user: User
}

export interface Sandbox {
  id: string
  runtime: string
  status: string
  ttl: number
  created_at: string
  port: number
  preview_url: string
  image: string
  worker_id: string | null
  vcpu_count: number
  mem_size_mib: number
  user_id: string | null
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(
  path: string,
  options: RequestInit & { token?: string | null } = {},
): Promise<T> {
  const { token, headers, ...rest } = options
  const res = await fetch(`${API_URL}${path}`, {
    ...rest,
    headers: {
      ...(rest.body ? { 'Content-Type': 'application/json' } : {}),
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...headers,
    },
  })

  if (!res.ok) {
    const text = await res.text().catch(() => '')
    throw new ApiError(res.status, text || `request failed: ${res.status}`)
  }

  if (res.status === 204 || res.status === 202) {
    return undefined as T
  }
  return (await res.json()) as T
}

export const api = {
  signUp: (email: string, password: string) =>
    request<AuthResponse>('/api/auth/signup', {
      method: 'POST',
      body: JSON.stringify({ email, password }),
    }),

  login: (email: string, password: string) =>
    request<AuthResponse>('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ email, password }),
    }),

  logout: (token: string) =>
    request<void>('/api/auth/logout', { method: 'POST', token }),

  listSandboxes: (token: string) =>
    request<Sandbox[]>('/api/sandboxes', { token }),

  getSandbox: (token: string, id: string) =>
    request<Sandbox>(`/api/sandboxes/${id}`, { token }),

  createSandbox: (
    token: string,
    input: { runtime: string; vcpu_count?: number; mem_size_mib?: number },
  ) =>
    request<Sandbox>('/api/sandboxes', {
      method: 'POST',
      token,
      body: JSON.stringify(input),
    }),

  deleteSandbox: (token: string, id: string) =>
    request<void>(`/api/sandboxes/${id}`, { method: 'DELETE', token }),
}
