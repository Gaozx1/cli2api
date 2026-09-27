import { useEffect, useMemo, useState } from 'react'
import { Button, Card, Chip, Input, Label, ListBox, Select, TextArea } from '@heroui/react'
import { CheckCircle, HandHeart, Warning } from '@phosphor-icons/react'
import { useI18n } from '@/hooks/useI18n'
import { fetchDonationInfo, submitDonation, type DonationFormat, type DonationInfo, type DonationResult } from '@/api/donations'
import { FormRow } from '@/components/ui/FormRow'
import { PageAlert } from '@/components/ui/PageAlert'
import { ProviderMark } from '@/components/ProviderMark'
import { accountProviderLabel } from '@/lib/provider'

type SubmitState = 'idle' | 'loading' | 'done' | 'error'

// The Qoder CLI stores its login as a base64 auth blob plus a machine id rather
// than a JSON credential, so that format gets its own pair of fields.
const QODER_NATIVE = 'qoder_native'

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
  const [credential, setCredential] = useState('')
  const [userBlob, setUserBlob] = useState('')
  const [machineID, setMachineID] = useState('')
  const [state, setState] = useState<SubmitState>('idle')
  const [message, setMessage] = useState('')
  const [result, setResult] = useState<DonationResult | null>(null)

  useEffect(() => {
    let cancelled = false
    fetchDonationInfo()
      .then((data) => {
        if (cancelled) return
        setInfo(data)
        const first = data.formats?.[0]
        if (first) setFormatID(first.format)
      })
      .catch((err: unknown) => {
        if (!cancelled) setInfoError(err instanceof Error ? err.message : String(err))
      })
    return () => {
      cancelled = true
    }
  }, [])

  const selected: DonationFormat | undefined = useMemo(
    () => info?.formats?.find((format) => format.format === formatID),
    [info, formatID],
  )

  const rewardUSD = info?.default_usd ?? 1
  const rewardQuota = info?.quota_per_usd ?? 0

  async function onSubmit() {
    const numericID = Number.parseInt(userID.trim(), 10)
    if (!Number.isFinite(numericID) || numericID <= 0) {
      setState('error')
      setMessage(t('donations.userIDInvalid'))
      return
    }
    if (!selected) {
      setState('error')
      setMessage(t('donations.formatRequired'))
      return
    }

    const body: Record<string, unknown> = {
      format: selected.format,
      name: accountName.trim() || undefined,
      region: selected.region || undefined,
      newapi_user_id: numericID,
      credit_usd: rewardUSD,
    }
    if (selected.credential_kind === QODER_NATIVE) {
      if (!userBlob.trim() || !machineID.trim()) {
        setState('error')
        setMessage(t('donations.qoderFieldsRequired'))
        return
      }
      body.user_blob = userBlob.trim()
      body.machine_id = machineID.trim()
    } else {
      const parsed = parseJSONCredential(credential)
      if (parsed.value === undefined) {
        setState('error')
        setMessage(parsed.error === 'empty' ? t('donations.credentialRequired') : t('donations.credentialInvalid'))
        return
      }
      body.credential = parsed.value
    }

    setState('loading')
    setMessage('')
    setResult(null)
    try {
      const data = await submitDonation(body as never)
      setResult(data)
      if (data.credited) {
        setState('done')
        setMessage(t('donations.success', { usd: data.credited_usd, quota: data.credited_quota }))
      } else {
        // The account was accepted but the credit did not land. Report it as a
        // partial outcome rather than a failure so the contributor knows the
        // contribution counted.
        setState('error')
        setMessage(t('donations.creditFailed', { error: data.credit_error || '' }))
      }
      setCredential('')
      setUserBlob('')
      setMachineID('')
    } catch (err: unknown) {
      setState('error')
      setMessage(err instanceof Error ? err.message : String(err))
    }
  }

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
              onChange={(value) => setFormatID(String(value ?? ''))}
            >
              <Select.Trigger>
                <Select.Value />
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
              onChange={(event) => setUserID(event.target.value)}
            />
          </FormRow>

          <FormRow label={t('donations.fieldName')} hint={t('donations.fieldNameHint')} htmlFor="donation-name">
            <Input
              id="donation-name"
              fullWidth
              value={accountName}
              onChange={(event) => setAccountName(event.target.value)}
            />
          </FormRow>

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

          <div className="flex items-center gap-3">
            <Button
              variant="secondary"
              isDisabled={state === 'loading' || !formatID}
              onPress={onSubmit}
            >
              {state === 'loading' ? t('donations.submitting') : t('donations.submit')}
            </Button>
            {state === 'done' ? <CheckCircle className="size-5 text-success" /> : null}
          </div>

          {infoError ? <PageAlert title={t('donations.infoFailed')} description={infoError} /> : null}
          {state === 'error' && message ? <PageAlert title={t('donations.failed')} description={message} /> : null}
          {state === 'done' && message ? <PageAlert status="success" title={t('donations.credited')} description={message} /> : null}
          {result && !result.credited ? (
            <PageAlert status="warning" title={t('donations.acceptedNoCredit')} />
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
