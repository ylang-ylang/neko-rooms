// Handwritten, typed companion to the generated API; uses the same management UI base.
import axios from 'axios'
import { installManagerAuthInterceptor, managerBasePath } from '@/auth'

export interface AgentKey {
  key_id: string;
  room_name: string;
  label: string;
  created_at: string;
  expires_at: string;
  active_connections: number;
}

export interface IssuedAgentKey {
  key_id: string;
  key: string;
  room_name: string;
  created_at: string;
  expires_at: string;
  cdp_url: string;
  prompt: string;
}

// Same-origin cookies (or browser-cached BasicAuth in basic mode); never set auth headers.
const api = axios.create({ timeout: 15000 })
installManagerAuthInterceptor(api)

function roomKeysUrl(roomName: string): string {
  return `${managerBasePath}/api/agent/rooms/${encodeURIComponent(roomName)}/keys`
}

export async function createAgentKey(roomName: string, ttlSeconds: number, label: string): Promise<IssuedAgentKey> {
  const response = await api.post<IssuedAgentKey>(roomKeysUrl(roomName), {
    ttl_seconds: ttlSeconds,
    ...(label.trim() ? { label: label.trim() } : {}),
  })
  return response.data
}

export async function listAgentKeys(roomName: string): Promise<AgentKey[]> {
  const response = await api.get<{ keys: AgentKey[] }>(roomKeysUrl(roomName))
  return response.data.keys
}

export async function revokeAgentKey(roomName: string, keyId: string): Promise<void> {
  await api.delete(`${roomKeysUrl(roomName)}/${encodeURIComponent(keyId)}`)
}

export function agentErrorMessage(error: unknown): string {
  if (axios.isAxiosError(error) && error.response) {
    const data: unknown = error.response.data
    let detail = ''
    if (typeof data === 'string') {
      detail = data.trim()
    } else if (data && typeof data === 'object') {
      const body = data as { message?: unknown; error?: unknown }
      if (typeof body.message === 'string') detail = body.message
      else if (typeof body.error === 'string') detail = body.error
    }
    return `HTTP ${error.response.status}${detail ? `: ${detail}` : ''}`
  }
  return error instanceof Error ? error.message : 'Network request failed. Please retry.'
}
