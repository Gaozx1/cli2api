import { useEffect, useMemo, useRef, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { Button, Card, Chip, Input, Label, ListBox, Select, TextArea } from '@heroui/react'
import { CheckCircle, HandHeart, Lock, Warning } from '@phosphor-icons/react'
import { useI18n } from '@/hooks/useI18n'
import {
  cancelDonationSession,
  completeDonationSession,
  fetchDonationInfo,
  pollDonationSession,
  restartDonationSession,
  startDonation,
  submitDonation,
  type DonationFormat,
  type DonationInfo,
  type DonationSession,
} from '@/api/donations'
import { FormRow } from '@/components/ui/FormRow'
import { PageAlert } from '@/components/ui/PageAlert'
import { ProviderMark } from '@/components/ProviderMark'
import { accountProviderLabel } from '@/lib/provider'

type Phase = 'idle' | 'starting' | 'waiting' | 'done' | 'error'

// The Qoder CLI stores its login as a base64 auth blob plus a machine id rather
// than a JSON credential, so that format gets its own pair of fields.
const QODER_NATIVE = 'qoder_native'
const POLL_INTERVAL = 2500
const POLL_ATTEMPTS = 120 // ~5 minutes, matching the pending-session TTL
// Shown only until the info request lands, and only as a unit symbol for a
// ratio: the real amount is always the server's default_usd, so the payout and
// this label cannot drift apart when the reward changes.
const FALLBACK_REWARD_USD = 1

// formatKey identifies a selectable entry. One provider can be offered in more
// than one region (Qoder global+cn, WorkBuddy cn+global), so the provider alone
// is not unique -- the same "provider:region" shape the keys page uses.
function formatKey(format: DonationFormat) {
  return `${format.provider}:${format.region}`
}

// uidFromQuery extracts a positive integer uid from a query string
// ("?uid=123&x=1" -> "123"). Anything absent, non-numeric or non-positive
// yields an empty string, which means "no link pinned a recipient".
function uidFromQuery(search: string): string {
  const raw = new URLSearchParams(search).get('uid') ?? ''
  const value = Number.parseInt(raw.trim(), 10)
  return Number.isFinite(value) && value > 0 ? String(value) : ''
}

function parseJSONCredential(raw: string): { value?: unknown; error?: string } {  const text = raw.trim()
  if (!text) return { error: 'empty' }
  try {
    return { value: JSON.parse(text) }
  } catch {
    return { error: 'invalid' }
  }
}

export function DonationsPage() {
  const { t } = useI18n()
  const navigate = useNavigate()
  const location = useLocation()
  // A contribution link can carry the contributor's New API user id. It is held
  // in state rather than derived from the query string, because the query string
  // is cleared below and the lock has to outlive it.
  const [presetUID, setPresetUID] = useState(() => uidFromQuery(window.location.search))
  const [info, setInfo] = useState<DonationInfo | null>(null)
  const [infoError, setInfoError] = useState('')
  const [formatID, setFormatID] = useState('')
  // The effective recipient: the preset id when a link pinned one, otherwise
  // whatever was typed. Read-only in the former case -- there is no input for
  // the contributor to change.
  const [userID, setUserID] = useState('')
  const [accountName, setAccountName] = useState('')
  const [phase, setPhase] = useState<Phase>('idle')
  const [message, setMessage] = useState('')
  const [session, setSession] = useState<DonationSession | null>(null)
  const [authUrl, setAuthUrl] = useState('')
  const [callbackUrl, setCallbackUrl] = useState('')
  // Fallback credential fields, for formats without a browser login.
  const [credential, setCredential] = useState('')
  const [userBlob, setUserBlob] = useState('')
  const [machineID, setMachineID] = useState('')
  const timer = useRef<number | null>(null)
  const cancelled = useRef(false)

  // Handle the uid query and clear it from the address bar.
  //
  // Keyed on location.search, NOT on mount: when the app is already showing
  // /donations and only the query changes (an in-app link, or a pasted URL the
  // router handles client-side), React Router keeps the same component mounted
  // and this effect would never re-run on a [] dependency -- the tail stayed in
  // the address bar and the field never locked. Keying on location.search also
  // means clearing the query re-runs it once more with an empty search, which is
  // harmless: the uid already lives in state.
  useEffect(() => {
    if (location.search) {
      const next = uidFromQuery(location.search)
      if (next) setPresetUID(next)
      navigate('/donations', { replace: true })
    }
  }, [location.search, navigate])

  useEffect(() => {
    let active = true
    fetchDonationInfo()
      .then((data) => {
        if (!active) return
        setInfo(data)
        const first = data.formats?.[0]
        if (first) setFormatID(formatKey(first))
      })
      .catch((err: unknown) => {
        if (active) setInfoError(err instanceof Error ? err.message : String(err))
      })
    return () => {
      active = false
    }
  }, [])

  // Stop polling when the page goes away so a detached round is not driven
  // after the contributor leaves.
  useEffect(() => {
    return () => {
      cancelled.current = true
      if (timer.current) window.clearTimeout(timer.current)
    }
  }, [])

  const selected: DonationFormat | undefined = useMemo(
    () => info?.formats?.find((format) => formatKey(format) === formatID),
    [info, formatID],
  )
  const webAuth = Boolean(selected?.web_auth)
  const needsCallback = Boolean(session?.callback_required ?? selected?.callback_required)
  const rewardUSD = info?.default_usd ?? FALLBACK_REWARD_USD
  const rewardQuota = info?.quota_per_usd ?? 0

  function numericUserID(): number | null {
    // The preset wins: when a link pinned the recipient there is no editable
    // value, and state can never disagree with the link that was opened.
    const value = Number.parseInt((presetUID || userID).trim(), 10)
    if (!Number.isFinite(value) || value <= 0) return null
    return value
  }

  // poll drives one authorization round to a terminal state, settling the
  // reward on the server exactly once.
  async function poll(id: string, attempt = 0): Promise<void> {
    if (cancelled.current) return
    if (attempt >= POLL_ATTEMPTS) {
      setPhase('error')
      setMessage(t('donations.authTimeout'))
      return
    }
    try {
      const current = await pollDonationSession(id)
      if (cancelled.current) return
      setSession(current)
      if (current.credited) {
        setPhase('done')
        setMessage(t('donations.success', { usd: current.credit_usd, quota: (current.credited_quota || 0).toLocaleString() }))
        return
      }
      if (current.status === 'failed') {
        setPhase('error')
        setMessage(current.credit_error ? t('donations.creditFailed', { error: current.credit_error }) : current.message || t('donations.failed'))
        return
      }
      // A provider that redirects to a loopback address the server cannot
      // receive never becomes done by polling: stop and wait for the pasted
      // callback URL instead.
      if (current.callback_required) {
        setMessage(current.message || t('donations.waitingCallback'))
        return
      }
      setMessage(current.message || t('donations.waitingAuth'))
      timer.current = window.setTimeout(() => void poll(id, attempt + 1), POLL_INTERVAL)
    } catch (err: unknown) {
      if (cancelled.current) return
      setPhase('error')
      setMessage(err instanceof Error ? err.message : String(err))
    }
  }

  // onRestart re-opens the provider login when the round's handshake was lost,
  // instead of leaving the contributor stuck on a dead authorization page.
  async function onRestart() {
    if (!session) return
    setPhase('starting')
    setMessage('')
    try {
      const fresh = await restartDonationSession(session.session_id)
      setSession(fresh)
      setAuthUrl(fresh.auth_url)
      if (fresh.auth_url) window.open(fresh.auth_url, '_blank', 'noopener,noreferrer')
      setPhase('waiting')
      setMessage(t('donations.waitingAuth'))
      cancelled.current = false
      void poll(fresh.session_id)
    } catch (err: unknown) {
      setPhase('error')
      setMessage(err instanceof Error ? err.message : String(err))
    }
  }
  // onCallback finishes a round with the URL the contributor copied out of
  // their browser, for providers whose redirect never reaches this server.
  async function onCallback() {
    if (!session) return
    if (!callbackUrl.trim()) {
      setPhase('error')
      setMessage(t('donations.callbackRequired'))
      return
    }
    setPhase('starting')
    try {
      const settled = await completeDonationSession(session.session_id, callbackUrl.trim())
      setSession(settled)
      if (settled.credited) {
        setPhase('done')
        setMessage(t('donations.success', { usd: settled.credit_usd, quota: (settled.credited_quota || 0).toLocaleString() }))
        setCallbackUrl('')
      } else if (settled.status === 'failed') {
        setPhase('error')
        setMessage(settled.credit_error ? t('donations.creditFailed', { error: settled.credit_error }) : settled.message || t('donations.failed'))
      } else {
        setPhase('waiting')
        setMessage(settled.message || t('donations.waitingAuth'))
      }
    } catch (err: unknown) {
      setPhase('error')
      setMessage(err instanceof Error ? err.message : String(err))
    }
  }

  async function onAuthorize() {
    const id = numericUserID()
    if (id === null) {
      setPhase('error')
      setMessage(t('donations.userIDInvalid'))
      return
    }
    if (!selected) {
      setPhase('error')
      setMessage(t('donations.formatRequired'))
      return
    }
    setPhase('starting')
    setMessage(t('donations.starting'))
    setSession(null)
    setAuthUrl('')
    try {
      const started = await startDonation({
        format: selected.format,
        name: accountName.trim() || undefined,
        region: selected.region || undefined,
        newapi_user_id: id,
        credit_usd: rewardUSD,
      })
      setSession(started)
      setAuthUrl(started.auth_url)
      if (started.auth_url) window.open(started.auth_url, '_blank', 'noopener,noreferrer')
      setPhase('waiting')
      setMessage(t('donations.waitingAuth'))
      cancelled.current = false
      void poll(started.session_id)
    } catch (err: unknown) {
      setPhase('error')
      setMessage(err instanceof Error ? err.message : String(err))
    }
  }

  async function onCancel() {
    if (!session) return
    cancelled.current = true
    if (timer.current) window.clearTimeout(timer.current)
    try {
      await cancelDonationSession(session.session_id)
    } catch {
      // Cancelling is best effort; a swept session is equivalent.
    }
    setPhase('idle')
    setMessage('')
    setSession(null)
    setAuthUrl('')
  }

  // The pasted-credential path stays available for providers without a login.
  async function onSubmitCredential() {
    const id = numericUserID()
    if (id === null) {
      setPhase('error')
      setMessage(t('donations.userIDInvalid'))
      return
    }
    if (!selected) {
      setPhase('error')
      setMessage(t('donations.formatRequired'))
      return
    }
    const body: Record<string, unknown> = {
      format: selected.format,
      name: accountName.trim() || undefined,
      region: selected.region || undefined,
      newapi_user_id: id,
      credit_usd: rewardUSD,
    }
    if (selected.credential_kind === QODER_NATIVE) {
      if (!userBlob.trim() || !machineID.trim()) {
        setPhase('error')
        setMessage(t('donations.qoderFieldsRequired'))
        return
      }
      body.user_blob = userBlob.trim()
      body.machine_id = machineID.trim()
    } else {
      const parsed = parseJSONCredential(credential)
      if (parsed.value === undefined) {
        setPhase('error')
        setMessage(parsed.error === 'empty' ? t('donations.credentialRequired') : t('donations.credentialInvalid'))
        return
      }
      body.credential = parsed.value
    }
    setPhase('starting')
    setMessage(t('donations.submitting'))
    try {
      const result = await submitDonation(body as never)
      if (result.credited) {
        setPhase('done')
        setMessage(t('donations.success', { usd: result.credited_usd, quota: result.credited_quota.toLocaleString() }))
      } else {
        setPhase('error')
        setMessage(t('donations.creditFailed', { error: result.credit_error || '' }))
      }
      setCredential('')
      setUserBlob('')
      setMachineID('')
    } catch (err: unknown) {
      setPhase('error')
      setMessage(err instanceof Error ? err.message : String(err))
    }
  }

  const busy = phase === 'starting' || phase === 'waiting'

  return (
    <div className="mx-auto w-full max-w-3xl space-y-6">
      <header className="space-y-2">
        <div className="flex items-center gap-2">
          <HandHeart className="size-5 text-primary" />
          <h1 className="text-xl font-semibold text-foreground">{t('donations.title')}</h1>
          <Chip size="sm" variant="soft">{t('donations.noLogin')}</Chip>
        </div>
        <p className="text-sm leading-6 text-muted">{t('donations.subtitle')}</p>
      </header>

      <Card className="p-5">
        <div className="space-y-4">
          <div className="rounded-lg bg-default/40 p-3 text-sm">
            <p className="font-medium text-foreground">{t('donations.rewardTitle')}</p>
            <p className="mt-1 text-muted">
              {t('donations.rewardBody', { usd: rewardUSD, quota: rewardQuota.toLocaleString() })}
            </p>
          </div>

          <FormRow label={t('donations.fieldType')} htmlFor="donation-format">
            <Select
              id="donation-format"
              fullWidth
              value={formatID}
              isDisabled={busy}
              onChange={(value) => setFormatID(String(value ?? ''))}
            >
              <Select.Trigger className="items-center">
                <Select.Value className="min-w-0 truncate" />
                <Select.Indicator />
              </Select.Trigger>
              <Select.Popover>
                <ListBox>
                  {(info?.formats || []).map((format) => (
                    <ListBox.Item key={formatKey(format)} id={formatKey(format)} textValue={accountProviderLabel(format.provider, format.region, t)}>
                      <span className="flex items-center gap-2">
                        <ProviderMark provider={format.provider} />
                        <Label className="truncate">{accountProviderLabel(format.provider, format.region, t)}</Label>
                      </span>
                      <ListBox.ItemIndicator />
                    </ListBox.Item>
                  ))}
                </ListBox>
              </Select.Popover>
            </Select>
          </FormRow>

          {presetUID ? (
            // No input at all: the recipient is fixed by the link that was used,
            // so there is nothing for the contributor to edit.
            <FormRow label={t('donations.fieldUserID')} hint={t('donations.fieldUserIDLocked')}>
              <div className="flex h-10 items-center gap-2 rounded-xl border border-border bg-surface-secondary px-3">
                <Lock size={14} weight="bold" className="shrink-0 text-muted" />
                <span className="mono truncate text-sm text-foreground">{presetUID}</span>
              </div>
            </FormRow>
          ) : (
            <FormRow label={t('donations.fieldUserID')} hint={t('donations.fieldUserIDHint')} htmlFor="donation-user-id">
              <Input
                id="donation-user-id"
                fullWidth
                inputMode="numeric"
                value={userID}
                placeholder="1"
                disabled={busy}
                onChange={(event) => setUserID(event.target.value)}
              />
            </FormRow>
          )}

          <FormRow label={t('donations.fieldName')} hint={t('donations.fieldNameHint')} htmlFor="donation-name">
            <Input
              id="donation-name"
              fullWidth
              value={accountName}
              disabled={busy}
              onChange={(event) => setAccountName(event.target.value)}
            />
          </FormRow>

          {webAuth ? (
            <div className="flex flex-wrap items-center gap-3">
              <Button variant="secondary" isDisabled={busy || !formatID} onPress={onAuthorize}>
                {phase === 'starting' ? t('donations.starting') : t('donations.authorize')}
              </Button>
              {phase === 'waiting' ? (
                <>
                  <Button variant="ghost" onPress={onCancel}>{t('donations.cancel')}</Button>
                  <Button variant="ghost" onPress={onRestart}>{t('donations.restartAuth')}</Button>
                </>
              ) : null}
              {authUrl ? (
                <a
                  className="text-sm text-primary underline-offset-2 hover:underline"
                  href={authUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  {t('donations.reopenAuth')}
                </a>
              ) : null}
            </div>
          ) : (
            <>
              {selected?.credential_kind === QODER_NATIVE ? (
                <>
                  <FormRow label={t('donations.fieldUserBlob')} hint={t('donations.fieldUserBlobHint')} htmlFor="donation-blob">
                    <TextArea
                      id="donation-blob"
                      fullWidth
                      rows={4}
                      value={userBlob}
                      onChange={(event) => setUserBlob(event.target.value)}
                    />
                  </FormRow>
                  <FormRow label={t('donations.fieldMachineID')} htmlFor="donation-machine">
                    <Input
                      id="donation-machine"
                      fullWidth
                      value={machineID}
                      onChange={(event) => setMachineID(event.target.value)}
                    />
                  </FormRow>
                </>
              ) : (
                <FormRow label={t('donations.fieldCredential')} hint={t('donations.fieldCredentialHint')} htmlFor="donation-credential">
                  <TextArea
                    id="donation-credential"
                    fullWidth
                    rows={6}
                    value={credential}
                    placeholder="{ ... }"
                    onChange={(event) => setCredential(event.target.value)}
                  />
                </FormRow>
              )}
              <Button variant="secondary" isDisabled={busy || !formatID} onPress={onSubmitCredential}>
                {phase === 'starting' ? t('donations.submitting') : t('donations.submit')}
              </Button>
            </>
          )}

          {needsCallback && session && phase !== 'done' ? (
            <div className="space-y-2">
              <p className="text-xs leading-5 text-muted">{t('donations.callbackLead')}</p>
              <TextArea
                fullWidth
                rows={3}
                value={callbackUrl}
                placeholder="https://.../?code=...&state=..."
                onChange={(event) => setCallbackUrl(event.target.value)}
              />
              <Button variant="secondary" isDisabled={phase === 'starting'} onPress={onCallback}>
                {t('donations.submitCallback')}
              </Button>
            </div>
          ) : null}

          {phase === 'waiting' ? (
            <p className="text-xs leading-5 text-muted">{t('donations.waitingHint')}</p>
          ) : null}

          {infoError ? <PageAlert title={t('donations.infoFailed')} description={infoError} /> : null}
          {phase === 'error' && message ? <PageAlert title={t('donations.failed')} description={message} /> : null}
          {phase === 'done' && message ? (
            <PageAlert status="success" title={t('donations.credited')} description={message} />
          ) : null}
          {message && phase !== 'error' && phase !== 'done' ? (
            <p className="flex items-center gap-2 text-sm text-muted">
              <CheckCircle className="size-4 text-muted" />
              {message}
            </p>
          ) : null}
        </div>
      </Card>

      <Card className="p-5">
        <div className="flex items-start gap-2">
          <Warning className="mt-0.5 size-4 shrink-0 text-muted" />
          <div className="space-y-1 text-xs leading-5 text-muted">
            <Label className="text-xs font-medium text-foreground">{t('donations.notesTitle')}</Label>
            <p>{t('donations.notesBody')}</p>
          </div>
        </div>
      </Card>
    </div>
  )
}
