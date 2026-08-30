import { useCallback, useEffect, useState, type FormEvent } from 'react'
import { useAuth } from '../auth/AuthContext'
import { api, ApiError, type Sandbox } from '../api/client'

const RUNTIMES = ['node', 'python', 'go']

export function Dashboard() {
  const { token, user, logout } = useAuth()
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [runtime, setRuntime] = useState(RUNTIMES[0])
  const [creating, setCreating] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    if (!token) return
    setLoading(true)
    setError(null)
    try {
      const list = await api.listSandboxes(token)
      setSandboxes(list ?? [])
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Failed to load sandboxes')
    } finally {
      setLoading(false)
    }
  }, [token])

  useEffect(() => {
    void refresh()
  }, [refresh])

  const onCreate = async (e: FormEvent) => {
    e.preventDefault()
    if (!token) return
    setCreating(true)
    setError(null)
    try {
      await api.createSandbox(token, { runtime })
      await refresh()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Failed to create sandbox')
    } finally {
      setCreating(false)
    }
  }

  const onDelete = async (id: string) => {
    if (!token) return
    setPendingDelete(id)
    setError(null)
    try {
      await api.deleteSandbox(token, id)
      await refresh()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Failed to delete sandbox')
    } finally {
      setPendingDelete(null)
    }
  }

  return (
    <div className="dashboard">
      <header className="dashboard-header">
        <h1>Sandboxes</h1>
        <div className="dashboard-user">
          <span>{user?.email}</span>
          <button type="button" onClick={logout}>
            Log out
          </button>
        </div>
      </header>

      <form className="create-form" onSubmit={onCreate}>
        <select value={runtime} onChange={(e) => setRuntime(e.target.value)}>
          {RUNTIMES.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
        <button type="submit" disabled={creating}>
          {creating ? 'Creating…' : 'New sandbox'}
        </button>
      </form>

      {error && <p className="form-error">{error}</p>}

      {loading ? (
        <p>Loading…</p>
      ) : sandboxes.length === 0 ? (
        <p className="empty-state">No sandboxes yet.</p>
      ) : (
        <table className="sandbox-table">
          <thead>
            <tr>
              <th>ID</th>
              <th>Runtime</th>
              <th>Status</th>
              <th>Worker</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {sandboxes.map((sbx) => (
              <tr key={sbx.id}>
                <td className="mono">{sbx.id}</td>
                <td>{sbx.runtime}</td>
                <td>
                  <span className={`status status-${sbx.status}`}>{sbx.status}</span>
                </td>
                <td className="mono">{sbx.worker_id ?? '—'}</td>
                <td>
                  <button
                    type="button"
                    onClick={() => onDelete(sbx.id)}
                    disabled={pendingDelete === sbx.id}
                  >
                    {pendingDelete === sbx.id ? 'Deleting…' : 'Delete'}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
