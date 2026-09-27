import { useEffect, useMemo, useRef, useState } from 'react'
import { Button, Card, Chip, Input, Label, ListBox, Select, TextArea } from '@heroui/react'
import { CheckCircle, HandHeart, Warning } from '@phosphor-icons/react'
import { useI18n } from '@/hooks/useI18n'
import {
  cancelDonationSession,
  fetchDonationInfo,
  pollDonationSession,
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

function parseJSONCredential(raw: string): { value?: unknown; error?: string } {
  const text = raw.trim()
  if (!text) return { error: 'empty' }
  try {
    return { value: JSON.parse(text) }
  } catch {
    return { error: 'invalid' }
  }
}

export function DonationsPage() {
  const { t } = useI18n()
  const [info, setInfo] = useState<DonationInfo | null>(null)
  const [infoError, setInfoError] = useState('')
  const [formatID, setFormatID] = useState('')
  const [userID, setUserID] = useState('')
  const [accountName, setAccountName] = useState('')
  const [phase, setPhase] = useState<Phase>('idle')
  const [message, setMessage] = useState('')
  const [session, setSession] = useState<DonationSession | null>(null)
  const [authUrl, setAuthUrl] = useState('')
  // Fallback credential fields, for formats without a browser login.
  const [credential, setCredential] = useState('')
  const [userBlob, setUserBlob] = useState('')
  const [machineID, setMachineID] = useState('')
  const timer = useRef<number | null>(null)
  const cancelled = useRef(false)

  useEffect(() => {
    let active = true
    fetchDonationInfo()
      .then((data) => {
        if (!active) return
        setInfo(data)
        const first = data.formats?.[0]
        if (first) setFormatID(first.format)
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
    () => info?.formats?.find((format) => format.format === formatID),
    [info, formatID],
  )
  const webAuth = Boolean(selected?.web_auth)
  const rewardUSD = info?.default_usd ?? 1
  const rewardQuota = info?.quota_per_usd ?? 0

  function numericUserID(): number | null {
    const value = Number.parseInt(userID.trim(), 10)
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
      setMessage(current.message || t('donations.waitingAuth'))
      timer.current = window.setTimeout(() => void poll(id, attempt + 1), POLL_INTERVAL)
    } catch (err: unknown) {
      if (cancelled.current) return
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
                    <ListBox.Item key={format.format} id={format.format} textValue={format.label}>
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
                <Button variant="ghost" onPress={onCancel}>{t('donations.cancel')}</Button>
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
