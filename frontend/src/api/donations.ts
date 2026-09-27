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
  description: string
}

export type DonationInfo = {
  object: string
  default_usd: number
  quota_per_usd: number
  formats: DonationFormat[]
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

export function submitDonation(input: DonationSubmit) {
  return api<DonationResult>('/api/donations', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}
