import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { api, type User } from '../api/client'

// Token lives in localStorage, not a cookie: the front (Vercel) and the API
// (a separate domain) are cross-origin, so a Bearer token sent explicitly
// avoids SameSite/CORS-credentials friction a cookie-based session would
// hit. See auth_http.go — sessions are opaque, revocable tokens, not JWTs.
const STORAGE_KEY = 'fire2_auth'

interface StoredAuth {
  token: string
  user: User
}

interface AuthContextValue {
  token: string | null
  user: User | null
  loading: boolean
  signUp: (email: string, password: string) => Promise<void>
  login: (email: string, password: string) => Promise<void>
  logout: () => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

function readStored(): StoredAuth | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    return raw ? (JSON.parse(raw) as StoredAuth) : null
  } catch {
    // Private browsing / storage disabled — fall back to "logged out".
    return null
  }
}

function writeStored(value: StoredAuth | null) {
  try {
    if (value) {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(value))
    } else {
      localStorage.removeItem(STORAGE_KEY)
    }
  } catch {
    // Ignore — nothing sensible to do if storage is unavailable.
  }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [auth, setAuth] = useState<StoredAuth | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    setAuth(readStored())
    setLoading(false)
  }, [])

  const signUp = async (email: string, password: string) => {
    const resp = await api.signUp(email, password)
    setAuth(resp)
    writeStored(resp)
  }

  const login = async (email: string, password: string) => {
    const resp = await api.login(email, password)
    setAuth(resp)
    writeStored(resp)
  }

  const logout = () => {
    const token = auth?.token
    setAuth(null)
    writeStored(null)
    if (token) {
      // Best-effort: revoke server-side, but the client is already logged
      // out regardless of whether this call succeeds.
      void api.logout(token).catch(() => {})
    }
  }

  return (
    <AuthContext.Provider
      value={{ token: auth?.token ?? null, user: auth?.user ?? null, loading, signUp, login, logout }}
    >
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) {
    throw new Error('useAuth must be used within an AuthProvider')
  }
  return ctx
}
