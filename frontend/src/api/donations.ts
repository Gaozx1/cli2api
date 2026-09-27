import { api } from './client'

// A contributable credential format advertised by the backend. The page renders
// these instead of hardcoding which providers accept donations, so adding a
// provider on the Go side is enough.
export type DonationFormat = {
  format: string
  provider: string
  label: string
  region: string
  // "json" for a credential object, "qoder_native" for the base64 auth blob
  // plus machine id pair the Qoder CLI writes to its own home.
  credential_kind: string
  // web_auth is true when the account can be authorized through the provider's
  // own browser login, which is the preferred flow.
  web_auth: boolean
  description: string
}

export type DonationInfo = {
  object: string
  default_usd: number
  quota_per_usd: number
  formats: DonationFormat[]
}

// DonationSession is one in-flight web authorization. The reward is credited
// only once status becomes "credited".
export type DonationSession = {
  session_id: string
  account_id: string
  provider: string
  region: string
  name: string
  format: string
  newapi_user_id: number
  credit_usd: number
  auth_url: string
  status: 'pending' | 'credited' | 'failed'
  message?: string
  credited: boolean
  credited_quota?: number
  credit_error?: string
}

export type DonationStart = {
  format: string
  name?: string
  region?: string
  newapi_user_id: number
  credit_usd?: number
}

export type DonationResult = {
  account_id: string
  account_name: string
  provider: string
  region: string
  status: string
  credited_usd: number
  credited_quota: number
  credited: boolean
  // Set when the account was accepted but the credit call failed. The
  // contribution still counts; an operator credits it manually.
  credit_error?: string
}

export type DonationSubmit = {
  format: string
  name?: string
  region?: string
  newapi_user_id: number
  credit_usd?: number
  credential?: unknown
  user_blob?: string
  machine_id?: string
}

// /api/donations is intentionally public, so these calls never send a console
// key and must not be wrapped in RequireAuth.
export function fetchDonationInfo() {
  return api<DonationInfo>('/api/donations')
}

// startDonation begins a web-authorization round and returns the URL to open.
export function startDonation(input: DonationStart) {
  return api<DonationSession>('/api/donations', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

export function pollDonationSession(sessionId: string) {
  return api<DonationSession>(`/api/donations/sessions/${encodeURIComponent(sessionId)}`)
}

export function cancelDonationSession(sessionId: string) {
  return api<void>(`/api/donations/sessions/${encodeURIComponent(sessionId)}`, { method: 'DELETE' })
}

// submitDonation is the legacy pasted-credential path, kept for providers that
// do not expose a browser login.
export function submitDonation(input: DonationSubmit) {
  return api<DonationResult>('/api/donations/credential', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}
